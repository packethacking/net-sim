package main

// Host CPU% sampler. Reads /proc/stat once per second, computes the
// busy fraction over the interval, and exposes it via cpuPct.Load().
// Linux-only; on other OSes Load returns -1.

import (
	"bufio"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type cpuSample struct {
	idle  uint64
	total uint64
}

// cpuPct holds the most recent CPU busy percentage as a float64 bit
// pattern. Initialised to NaN until the first sample completes.
var cpuPct atomic.Uint64

func init() {
	cpuPct.Store(math.Float64bits(math.NaN()))
	if runtime.GOOS != "linux" {
		return
	}
	go cpuSampler()
}

func currentCPUPct() float64 {
	return math.Float64frombits(cpuPct.Load())
}

func cpuSampler() {
	prev, ok := readProcStat()
	if !ok {
		return
	}
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	for range tick.C {
		cur, ok := readProcStat()
		if !ok {
			continue
		}
		dt := cur.total - prev.total
		di := cur.idle - prev.idle
		prev = cur
		if dt == 0 {
			continue
		}
		busyPct := 100.0 * float64(dt-di) / float64(dt)
		cpuPct.Store(math.Float64bits(busyPct))
	}
}

// readProcStat returns the cumulative idle and total CPU jiffies from
// /proc/stat's aggregated "cpu " line.
func readProcStat() (cpuSample, bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuSample{}, false
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	if !s.Scan() {
		return cpuSample{}, false
	}
	line := s.Text()
	if !strings.HasPrefix(line, "cpu ") {
		return cpuSample{}, false
	}
	fields := strings.Fields(line)[1:]
	var total, idle uint64
	for i, fv := range fields {
		v, err := strconv.ParseUint(fv, 10, 64)
		if err != nil {
			return cpuSample{}, false
		}
		total += v
		// Index 3 = idle, 4 = iowait — both count as "not busy".
		if i == 3 || i == 4 {
			idle += v
		}
	}
	return cpuSample{idle: idle, total: total}, true
}
