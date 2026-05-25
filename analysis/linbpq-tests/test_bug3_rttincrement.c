/*
 * Test for Bug 3: RTTIncrement ignores NeighbourSRTT.
 *
 * Compile:
 *   gcc -DBUGGY -o test_bug3_xfail test_bug3_rttincrement.c && ./test_bug3_xfail
 *   gcc         -o test_bug3_fixed test_bug3_rttincrement.c && ./test_bug3_fixed
 */

#include <stdio.h>

struct ROUTE {
    int SRTT;
    int NeighbourSRTT;
    int RTTIncrement;
};

static void compute_rttincrement(struct ROUTE *Route) {
#ifdef BUGGY
    Route->RTTIncrement = Route->SRTT / 2;
#else
    if (Route->NeighbourSRTT)
        Route->RTTIncrement = (Route->SRTT + Route->NeighbourSRTT) / 2;
    else
        Route->RTTIncrement = Route->SRTT / 2;
#endif
    if (Route->RTTIncrement == 0)
        Route->RTTIncrement = 1;
}

int main(void) {
    struct ROUTE route;
    int failures = 0;

    /* Test 1: both SRTT and NeighbourSRTT set */
    route.SRTT = 100;           /* 1 second in 10ms units */
    route.NeighbourSRTT = 200;  /* 2 seconds in 10ms units */
    route.RTTIncrement = 0;
    compute_rttincrement(&route);

    /* Fixed: (100+200)/2 = 150.  Buggy: 100/2 = 50. */
    if (route.RTTIncrement == 150) {
        printf("PASS: RTTIncrement = %d (averaged local + remote SRTT)\n",
               route.RTTIncrement);
    } else if (route.RTTIncrement == 50) {
        printf("XFAIL: RTTIncrement = %d (only used local SRTT/2, ignored NeighbourSRTT)\n",
               route.RTTIncrement);
        failures++;
    } else {
        printf("FAIL: RTTIncrement = %d (unexpected)\n", route.RTTIncrement);
        failures++;
    }

    /* Test 2: NeighbourSRTT is zero (not yet measured) — should fall back to SRTT/2 */
    route.SRTT = 80;
    route.NeighbourSRTT = 0;
    route.RTTIncrement = 0;
    compute_rttincrement(&route);

    if (route.RTTIncrement == 40) {
        printf("PASS: RTTIncrement = %d (fallback to SRTT/2 when no neighbour data)\n",
               route.RTTIncrement);
    } else {
        printf("FAIL: RTTIncrement = %d (expected 40)\n", route.RTTIncrement);
        failures++;
    }

    printf("\n%d test(s) failed\n", failures);
    return failures > 0 ? 1 : 0;
}
