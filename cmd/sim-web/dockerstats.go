package main

// Sim-only CPU% aggregator.
//
// Talks to the host's Docker daemon via the Unix socket (mounted into
// the container at /var/run/docker.sock — see deployment compose for
// the mount). On each tick:
//
//   1. resolves our own container by HOSTNAME → /containers/{id}/json
//      to learn which Docker networks we're on,
//   2. enumerates all running containers, keeps those sharing at least
//      one of those networks,
//   3. requests a stats snapshot per container (the snapshot embeds
//      precpu_stats so % is computable from a single call),
//   4. publishes the summed CPU% to simCPUPct.
//
// Sum exceeds 100% on multi-CPU hosts when the sim is using more than
// one core; that's the intended interpretation ("how busy is the sim
// across all its containers, scaled by the host's parallelism").

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// simCPUPct stores the most recent aggregate CPU% (across all sim
// containers) as a float64 bit pattern. NaN until first reading.
var simCPUPct atomic.Uint64

// dockerSockPath is overridable for tests; the production default is
// the canonical host socket location.
const dockerSockPath = "/var/run/docker.sock"

func init() {
	simCPUPct.Store(math.Float64bits(math.NaN()))
	if _, err := os.Stat(dockerSockPath); err != nil {
		// No daemon socket reachable → we just leave simCPUPct as NaN.
		// /api/stats will report NaN and the visualiser will hide the
		// number.
		return
	}
	go simCPUSampler()
}

func currentSimCPUPct() float64 {
	return math.Float64frombits(simCPUPct.Load())
}

// dockerClient is a tiny HTTP-over-unix-socket client for Docker.
// Reusing http.Client gives us connection pooling for free.
type dockerClient struct {
	hc *http.Client
}

func newDockerClient() *dockerClient {
	return &dockerClient{hc: &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", dockerSockPath)
			},
		},
	}}
}

func (c *dockerClient) get(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("docker %s: HTTP %d: %s", path, resp.StatusCode, body)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// inspectSelf returns the current container's networks. If we can't
// figure out our own container id, returns an error and the sampler
// gives up for this tick.
func (c *dockerClient) inspectSelf(ctx context.Context) (networks []string, err error) {
	host, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	var resp struct {
		NetworkSettings struct {
			Networks map[string]struct{} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := c.get(ctx, "/containers/"+host+"/json", &resp); err != nil {
		return nil, err
	}
	for n := range resp.NetworkSettings.Networks {
		networks = append(networks, n)
	}
	return networks, nil
}

// peers returns all running container IDs (including self) attached to
// at least one of the given networks.
func (c *dockerClient) peers(ctx context.Context, ourNets []string) ([]string, error) {
	netSet := make(map[string]struct{}, len(ourNets))
	for _, n := range ourNets {
		netSet[n] = struct{}{}
	}
	var resp []struct {
		ID         string `json:"Id"`
		NetworkSettings struct {
			Networks map[string]struct{} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := c.get(ctx, "/containers/json", &resp); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(resp))
	for _, c := range resp {
		for n := range c.NetworkSettings.Networks {
			if _, ok := netSet[n]; ok {
				out = append(out, c.ID)
				break
			}
		}
	}
	return out, nil
}

// statsSnapshot is the subset of Docker's container-stats payload we
// need to compute a single-shot CPU% number.
type statsSnapshot struct {
	CPUStats    cpuStatsBlock `json:"cpu_stats"`
	PreCPUStats cpuStatsBlock `json:"precpu_stats"`
}

type cpuStatsBlock struct {
	CPUUsage struct {
		TotalUsage uint64 `json:"total_usage"`
	} `json:"cpu_usage"`
	SystemUsage uint64 `json:"system_cpu_usage"`
	OnlineCPUs  uint64 `json:"online_cpus"`
}

func (c *dockerClient) containerCPUPct(ctx context.Context, id string) (float64, error) {
	var s statsSnapshot
	if err := c.get(ctx, "/containers/"+id+"/stats?stream=false&one-shot=false", &s); err != nil {
		return 0, err
	}
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(s.CPUStats.SystemUsage) - float64(s.PreCPUStats.SystemUsage)
	if cpuDelta <= 0 || sysDelta <= 0 {
		// First-tick situation: precpu_stats may be zero for short-lived
		// containers. Treat as 0% rather than NaN — the next tick has
		// real numbers.
		return 0, nil
	}
	cpus := float64(s.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = 1
	}
	return (cpuDelta / sysDelta) * cpus * 100, nil
}

// simCPUSampler runs forever in the background, refreshing simCPUPct
// every 3 s. Errors are silently absorbed — the visualiser will see
// either a stale value or NaN, both of which it handles.
func simCPUSampler() {
	cli := newDockerClient()
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for range tick.C {
		// 5 s per tick is plenty: Docker's `?stream=false&one-shot=false`
		// blocks ~1 s per container while it computes a delta against a
		// fresh snapshot. With the per-container calls fanned out below,
		// the whole sample should land in ~1 s regardless of container
		// count.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		total, err := cli.sampleOnce(ctx)
		cancel()
		if err != nil {
			continue
		}
		simCPUPct.Store(math.Float64bits(total))
	}
}

func (c *dockerClient) sampleOnce(ctx context.Context) (float64, error) {
	nets, err := c.inspectSelf(ctx)
	if err != nil {
		return 0, err
	}
	if len(nets) == 0 {
		return 0, errors.New("self not on any network")
	}
	ids, err := c.peers(ctx, nets)
	if err != nil {
		return 0, err
	}
	// Fan out per-container CPU% queries in parallel. The Docker stats
	// call for `?stream=false&one-shot=false` blocks for ~1 s on each
	// container while the daemon assembles the delta; running them
	// sequentially produced wall-clock time = N seconds and most calls
	// were cancelled by the per-tick context. With parallel calls the
	// whole sample finishes in ~1 s regardless of container count.
	results := make([]float64, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		i, id := i, id
		wg.Add(1)
		go func() {
			defer wg.Done()
			pct, err := c.containerCPUPct(ctx, id)
			if err == nil {
				results[i] = pct
			}
		}()
	}
	wg.Wait()
	var total float64
	for _, v := range results {
		total += v
	}
	return total, nil
}
