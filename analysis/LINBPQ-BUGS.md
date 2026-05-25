# LinBPQ Bug Report: 7 Bugs in NET/ROM and INP3 Routing

This document describes 7 bugs found in LinBPQ's routing implementation during
quantitative testing with [net-sim](https://github.com/packethacking/net-sim).
Each bug includes the exact location, a description, a fix, and a test strategy.

Patches for all bugs are in `analysis/linbpq-patches/`:
- `inp3-bugfixes-L3Code.patch` — fixes bugs 1 and 2
- `inp3-bugfixes-BPQINP3.patch` — fixes bugs 3–7

These bugs were validated by running a 10-node simulated packet radio network
and measuring convergence time and routing behavior. Stock LinBPQ with INP3
enabled converges in 219.9s; with all 7 bugs fixed, convergence drops to 109.9s
(-50%) and total frame count drops 35%.

---

## Bug 1 — CRITICAL: Assignment instead of comparison in L3TRYNEXTDEST

**File:** `L3Code.c`  
**Line:** 1460  
**Severity:** Critical — breaks route failover for ALL routes (NR and INP3)  
**Suggested branch:** `fix/l3-trynextdest-assignment-vs-comparison`

### The bug

```c
DEST->DEST_ROUTE++;         // TO NEXT

if (DEST->DEST_ROUTE = 7)   // BUG: = is assignment, not ==
    DEST->DEST_ROUTE = 1;   // TRY TO ACTIVATE FIRST
```

The single `=` always assigns 7 to `DEST_ROUTE`, the `if` evaluates the
assigned value (7, truthy), and the body unconditionally resets to 1. The
intended logic is to wrap back to route 1 only after all 6 slots have been
tried (3 NR routes + 3 INP3 routes).

### Impact

Route failover never advances past the first route. When a link fails,
`L3TRYNEXTDEST` always resets `DEST_ROUTE` to 1 instead of trying routes
2, 3, 4, 5, 6 in sequence. Multi-path resilience and INP3 fallback are
silently disabled on every node.

### Fix

```c
if (DEST->DEST_ROUTE == 7)
    DEST->DEST_ROUTE = 1;
```

### Test strategy

Write a C test that:
1. Creates a `DEST_LIST` with routes in slots 1 and 2
2. Sets `DEST_ROUTE = 1`, simulates the increment at line 1458
3. Calls the wrap-around check
4. **Expected (fixed):** `DEST_ROUTE == 2` (advanced to next route)
5. **Actual (buggy):** `DEST_ROUTE == 1` (always resets)

The test should build a minimal harness that includes `asmstrucs.h` and
exercises this specific logic path. It does not need a running LinBPQ
instance — just the data-structure manipulation.

```c
// test_l3_trynextdest_wraparound.c
#include <assert.h>
#include <string.h>
#include <stdio.h>

// Minimal reproduction of the DEST_LIST route wraparound logic
typedef unsigned char UCHAR;

struct NR_DEST_ROUTE_ENTRY {
    void *ROUT_NEIGHBOUR;
    UCHAR ROUT_QUALITY;
    UCHAR ROUT_OBSCOUNT;
    UCHAR ROUT_LOCKED;
};

struct INP3_DEST_ROUTE_ENTRY {
    void *ROUT_NEIGHBOUR;
    unsigned short STT;
    UCHAR Hops;
};

struct DEST_LIST {
    UCHAR DEST_ROUTE;
    struct NR_DEST_ROUTE_ENTRY NRROUTE[3];
    struct INP3_DEST_ROUTE_ENTRY INP3ROUTE[3];
};

// This mirrors L3TRYNEXTDEST's wrap logic at line 1458-1461
static void trynextdest_wrap(struct DEST_LIST *DEST) {
    DEST->DEST_ROUTE++;
#ifdef BUGGY
    if (DEST->DEST_ROUTE = 7)     // BUG: assignment
#else
    if (DEST->DEST_ROUTE == 7)    // FIX: comparison
#endif
        DEST->DEST_ROUTE = 1;
}

int main(void) {
    struct DEST_LIST dest;
    int passed = 0, failed = 0;

    // Test 1: route 1 should advance to 2
    memset(&dest, 0, sizeof(dest));
    dest.DEST_ROUTE = 1;
    trynextdest_wrap(&dest);
    if (dest.DEST_ROUTE == 2) {
        printf("PASS: route 1 -> 2\n");
        passed++;
    } else {
        printf("XFAIL: route 1 -> %d (expected 2)\n", dest.DEST_ROUTE);
        failed++;
    }

    // Test 2: route 5 should advance to 6
    dest.DEST_ROUTE = 5;
    trynextdest_wrap(&dest);
    if (dest.DEST_ROUTE == 6) {
        printf("PASS: route 5 -> 6\n");
        passed++;
    } else {
        printf("XFAIL: route 5 -> %d (expected 6)\n", dest.DEST_ROUTE);
        failed++;
    }

    // Test 3: route 6 should wrap to 1
    dest.DEST_ROUTE = 6;
    trynextdest_wrap(&dest);
    if (dest.DEST_ROUTE == 1) {
        printf("PASS: route 6 -> 1 (wraparound)\n");
        passed++;
    } else {
        printf("XFAIL: route 6 -> %d (expected 1)\n", dest.DEST_ROUTE);
        failed++;
    }

    printf("\n%d passed, %d failed\n", passed, failed);
    return failed > 0 ? 1 : 0;
}
```

Compile with `-DBUGGY` to see the xfail, without it to see the fix pass:
```bash
gcc -DBUGGY -o test_bug1_xfail test_l3_trynextdest_wraparound.c && ./test_bug1_xfail
gcc         -o test_bug1_fixed test_l3_trynextdest_wraparound.c && ./test_bug1_fixed
```

---

## Bug 2 — CRITICAL: Wrong array indexing in CLEARACTIVEROUTE

**File:** `L3Code.c`  
**Line:** 1025  
**Severity:** Critical — active routes are never properly cleared on link failure  
**Suggested branch:** `fix/clearactiveroute-array-index`

### The bug

```c
if (DEST->INP3ROUTE[DEST->DEST_ROUTE].ROUT_NEIGHBOUR == ROUTE)
```

`DEST_ROUTE` values 1–3 represent NR routes (stored in `NRROUTE[0–2]`),
and values 4–6 represent INP3 routes (stored in `INP3ROUTE[0–2]`). But
this code always indexes `INP3ROUTE[]` regardless of route type:

| DEST_ROUTE | Code accesses | Should access |
|:----------:|:-------------:|:-------------:|
| 1 | INP3ROUTE[1] | NRROUTE[0] |
| 2 | INP3ROUTE[2] | NRROUTE[1] |
| 3 | **INP3ROUTE[3]** (OOB!) | NRROUTE[2] |
| 4 | **INP3ROUTE[4]** (OOB!) | INP3ROUTE[0] |

### Impact

When a link goes down, `CLEARACTIVEROUTE` fails to match the active route
to the failed neighbour. Stale route pointers remain, preventing route
re-evaluation. The node continues trying to use the dead route.

### Fix

```c
if (DEST->DEST_ROUTE >= 1 && DEST->DEST_ROUTE <= 3
    ? DEST->NRROUTE[DEST->DEST_ROUTE - 1].ROUT_NEIGHBOUR == ROUTE
    : (DEST->DEST_ROUTE >= 4 && DEST->DEST_ROUTE <= 6
        ? DEST->INP3ROUTE[DEST->DEST_ROUTE - 4].ROUT_NEIGHBOUR == ROUTE
        : 0))
```

### Test strategy

```c
// test_clearactiveroute_index.c
#include <assert.h>
#include <string.h>
#include <stdio.h>

typedef unsigned char UCHAR;

struct ROUTE { char name[8]; };

struct NR_DEST_ROUTE_ENTRY {
    struct ROUTE *ROUT_NEIGHBOUR;
    UCHAR ROUT_QUALITY;
    UCHAR ROUT_OBSCOUNT;
    UCHAR ROUT_LOCKED;
};

struct INP3_DEST_ROUTE_ENTRY {
    struct ROUTE *ROUT_NEIGHBOUR;
    unsigned short STT;
    UCHAR Hops;
};

struct DEST_LIST {
    UCHAR DEST_ROUTE;
    struct NR_DEST_ROUTE_ENTRY NRROUTE[3];
    struct INP3_DEST_ROUTE_ENTRY INP3ROUTE[3];
};

static int check_active_route_buggy(struct DEST_LIST *DEST, struct ROUTE *ROUTE) {
    return DEST->INP3ROUTE[DEST->DEST_ROUTE].ROUT_NEIGHBOUR == ROUTE;
}

static int check_active_route_fixed(struct DEST_LIST *DEST, struct ROUTE *ROUTE) {
    if (DEST->DEST_ROUTE >= 1 && DEST->DEST_ROUTE <= 3)
        return DEST->NRROUTE[DEST->DEST_ROUTE - 1].ROUT_NEIGHBOUR == ROUTE;
    if (DEST->DEST_ROUTE >= 4 && DEST->DEST_ROUTE <= 6)
        return DEST->INP3ROUTE[DEST->DEST_ROUTE - 4].ROUT_NEIGHBOUR == ROUTE;
    return 0;
}

int main(void) {
    struct DEST_LIST dest;
    struct ROUTE routeA = {"RouteA"};
    int passed = 0, failed = 0;

    memset(&dest, 0, sizeof(dest));

    // Place routeA in NRROUTE[0] (DEST_ROUTE=1)
    dest.NRROUTE[0].ROUT_NEIGHBOUR = &routeA;
    dest.DEST_ROUTE = 1;

#ifdef BUGGY
    // Buggy: looks in INP3ROUTE[1] instead of NRROUTE[0]
    if (check_active_route_buggy(&dest, &routeA)) {
        printf("XFAIL: buggy code matched (should not happen unless memory overlap)\n");
        failed++;
    } else {
        printf("XFAIL: buggy code failed to find routeA in NRROUTE[0] when DEST_ROUTE=1\n");
        failed++;
    }
#else
    if (check_active_route_fixed(&dest, &routeA)) {
        printf("PASS: fixed code found routeA in NRROUTE[0] when DEST_ROUTE=1\n");
        passed++;
    } else {
        printf("FAIL: fixed code did not find routeA\n");
        failed++;
    }
#endif

    // Place routeA in INP3ROUTE[0] (DEST_ROUTE=4)
    memset(&dest, 0, sizeof(dest));
    dest.INP3ROUTE[0].ROUT_NEIGHBOUR = &routeA;
    dest.DEST_ROUTE = 4;

#ifdef BUGGY
    // Buggy: looks in INP3ROUTE[4] — out of bounds!
    printf("XFAIL: buggy code accesses INP3ROUTE[4] (out of bounds) for DEST_ROUTE=4\n");
    failed++;
#else
    if (check_active_route_fixed(&dest, &routeA)) {
        printf("PASS: fixed code found routeA in INP3ROUTE[0] when DEST_ROUTE=4\n");
        passed++;
    } else {
        printf("FAIL: fixed code did not find routeA\n");
        failed++;
    }
#endif

    printf("\n%d passed, %d failed\n", passed, failed);
    return failed > 0 ? 1 : 0;
}
```

---

## Bug 3 — SIGNIFICANT: RTTIncrement ignores NeighbourSRTT

**File:** `BPQINP3.c`  
**Line:** 376  
**Severity:** Significant — INP3 link transit time is underestimated  
**Suggested branch:** `fix/inp3-rttincrement-neighbour-srtt`

### The bug

```c
Route->RTTIncrement = Route->SRTT / 2;     // Half for one way time.
```

The comment in `asmstrucs.h` line 245 says RTTIncrement is:
> "Average of Ours and Neighbours SRTT in 10 ms"

But `NeighbourSRTT` (stored at line 960 when processing RTT messages) is
never used in this calculation. The result is that RTTIncrement only
reflects the local node's view of the round-trip time, not accounting for
the remote node's processing/queuing delay.

### Fix

```c
if (Route->NeighbourSRTT)
    Route->RTTIncrement = (Route->SRTT + Route->NeighbourSRTT) / 2;
else
    Route->RTTIncrement = Route->SRTT / 2;
```

### Test strategy

Create a test that sets up a ROUTE with known SRTT and NeighbourSRTT values,
then verifies RTTIncrement is computed correctly. With the bug, only SRTT/2
is used; with the fix, the average of both is used.

---

## Bug 4 — SIGNIFICANT: Route pointer corruption in sendAlltoOneNeigbour

**File:** `BPQINP3.c`  
**Line:** 1800  
**Severity:** Significant — corrupts function parameter, sends RIFs to wrong neighbour  
**Suggested branch:** `fix/inp3-sendall-route-corruption`

### The bug

```c
if (memcmp(Route->NEIGHBOUR_CALL, Dest->DEST_CALL, 7) == 0)
{
    if (DEBUGINP3) Debugprintf("INP3 Timer RIF Don't send %s to itself", Call);
    Route++;       // BUG: increments the function PARAMETER
    continue;
}
```

The function iterates over destinations (via the `i`/`Dest` loop) for a
single `Route` parameter. When skipping a self-referential entry, it
increments `Route`, which is the function parameter — not a loop variable.
All subsequent iterations use the wrong Route pointer, sending RIF entries
to an adjacent neighbour in memory.

### Fix

Remove the `Route++;` line. The `continue` alone skips to the next `Dest`.

```c
if (memcmp(Route->NEIGHBOUR_CALL, Dest->DEST_CALL, 7) == 0)
{
    if (DEBUGINP3) Debugprintf("INP3 Timer RIF Don't send %s to itself", Call);
    continue;
}
```

### Test strategy

Create a test with 2+ neighbours and verify that after skipping a
self-referential entry, subsequent RIF entries are still addressed to the
original Route, not Route+1.

---

## Bug 5 — SIGNIFICANT: Wrong RouteLastTT index in periodic refresh

**File:** `BPQINP3.c`  
**Lines:** 1806, 1808 (in `sendAlltoOneNeigbour`) and 1547 (in `SendRIFToNewNeighbour`)  
**Severity:** Significant — change detection for RIF refresh is broken  
**Suggested branch:** `fix/inp3-routelasttt-index`

### The bug

```c
// In sendAlltoOneNeigbour (lines 1806-1808):
lastTT = Dest->RouteLastTT[Entry->ROUT_NEIGHBOUR->recNum];     // BUG
Dest->RouteLastTT[Entry->ROUT_NEIGHBOUR->recNum] = sendTT;     // BUG

// In SendRIFToNewNeighbour (line 1547):
Dest->RouteLastTT[Entry->ROUT_NEIGHBOUR->recNum] = sendTT;     // BUG
```

`RouteLastTT` tracks the last travel-time value sent TO each neighbour,
indexed by the neighbour's `recNum`. But these lines use
`Entry->ROUT_NEIGHBOUR->recNum` — the index of the neighbour the route
was LEARNED FROM — instead of `Route->recNum` — the neighbour being SENT TO.

Compare with `SendRIFToOtherNeighbours` (lines 1417, 1467) which correctly
uses `Routes->recNum`.

### Fix

```c
// sendAlltoOneNeigbour:
lastTT = Dest->RouteLastTT[Route->recNum];
Dest->RouteLastTT[Route->recNum] = sendTT;

// SendRIFToNewNeighbour:
Dest->RouteLastTT[Route->recNum] = sendTT;
```

### Test strategy

Create a test with 3 neighbours (A, B, C). Learn a route from B, then call
the periodic refresh for neighbour C. Verify that `RouteLastTT[C.recNum]`
is updated, not `RouteLastTT[B.recNum]`.

---

## Bug 6 — MINOR: Dead unsigned < 0 check in ProcessRTTReply

**File:** `BPQINP3.c`  
**Line:** 361  
**Severity:** Minor — dead code, no functional impact  
**Suggested branch:** `fix/inp3-unsigned-comparison`

### The bug

```c
uint32_t RTT;
// ...
if (RTT > 60000 || RTT < 0)
    return;
```

`RTT` is `uint32_t` (unsigned 32-bit). The `RTT < 0` comparison is always
false. If a timer wrap causes a large unsigned result, the `> 60000` check
catches it, so this is harmless but misleading.

The comment on line 359 also says "We work internally in mS" but
`GetTickCountINP3()` returns 10ms units.

### Fix

```c
if (RTT > 60000)
    return;
```

And fix the comment to say `// 10ms units`.

---

## Bug 7 — MINOR: Redundant ROUTEPTR++ in UpdateNode loop

**File:** `BPQINP3.c`  
**Line:** 692  
**Severity:** Minor — dead code, no functional impact  
**Suggested branch:** `fix/inp3-redundant-routeptr-increment`

### The bug

```c
for (i = 0; i < 3; i++)
{
    ROUTEPTR = &Dest->INP3ROUTE[i];   // set from loop var
    if (ROUTEPTR->ROUT_NEIGHBOUR == NULL)
    {
        AddHere(ROUTEPTR, Route, hops, rtt);
        // ...
        return;
    }
    ROUTEPTR++;   // BUG: redundant — overwritten at top of next iteration
}
```

`ROUTEPTR` is reassigned from `&Dest->INP3ROUTE[i]` at the top of each
loop iteration, so the `ROUTEPTR++` is dead code.

### Fix

Remove the `ROUTEPTR++;` line.

---

## Measured Impact

All measurements use a 10-node 1200-baud simulated network with 4 multi-port
backbone nodes (VHF + UHF), interference links between distant VHF stations,
and `NODESINTERVAL=1` / `PREFERINP3ROUTES=1` / `ENABLEINP3` on backbone ports.

| Configuration | Convergence (s) | Total Frames | NODES % |
|:--------------|:---------------:|:------------:|:-------:|
| Baseline (NET/ROM only, no INP3) | 94.3 | 720 | 8.6% |
| Stock LinBPQ with INP3 enabled | **219.9** | 691 | 9.6% |
| LinBPQ with all 7 bugs fixed + INP3 | **109.9** | **451** | 10.6% |

Stock INP3 is **2.3× slower** than baseline because bugs 1 and 2 break
route failover for all routing, not just INP3. With all fixes applied,
INP3 converges 16% slower than baseline but uses **37% fewer frames** —
a significant efficiency improvement from the event-driven RIF mechanism.

## Reproduction

The simulation infrastructure and test scripts are at:
https://github.com/packethacking/net-sim/tree/claude/netrom-analysis-optimization-QQMlP/analysis

```bash
# Run stock INP3 experiment
python3 analysis/tools/run_experiment.py inp3-stock \
    --image linbpq-test:inp3-stock \
    --compose analysis/docker-compose-v2.yml

# Run fixed INP3 experiment
python3 analysis/tools/run_experiment.py inp3-fixed \
    --image linbpq-test:inp3-fixed \
    --compose analysis/docker-compose-v2.yml
```
