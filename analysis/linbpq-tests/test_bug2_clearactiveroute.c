/*
 * Test for Bug 2: CLEARACTIVEROUTE indexes INP3ROUTE[] when it should
 * index NRROUTE[] for DEST_ROUTE values 1-3.
 *
 * Compile:
 *   gcc -DBUGGY -o test_bug2_xfail test_bug2_clearactiveroute.c && ./test_bug2_xfail
 *   gcc         -o test_bug2_fixed test_bug2_clearactiveroute.c && ./test_bug2_fixed
 */

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

static int is_active_route(struct DEST_LIST *DEST, struct ROUTE *ROUTE) {
#ifdef BUGGY
    return DEST->INP3ROUTE[DEST->DEST_ROUTE].ROUT_NEIGHBOUR == ROUTE;
#else
    if (DEST->DEST_ROUTE >= 1 && DEST->DEST_ROUTE <= 3)
        return DEST->NRROUTE[DEST->DEST_ROUTE - 1].ROUT_NEIGHBOUR == ROUTE;
    if (DEST->DEST_ROUTE >= 4 && DEST->DEST_ROUTE <= 6)
        return DEST->INP3ROUTE[DEST->DEST_ROUTE - 4].ROUT_NEIGHBOUR == ROUTE;
    return 0;
#endif
}

int main(void) {
    struct DEST_LIST dest;
    struct ROUTE routeA = {"A"};
    struct ROUTE routeB = {"B"};
    int failures = 0;

    /* Test 1: NR route in slot 0, DEST_ROUTE=1 */
    memset(&dest, 0, sizeof(dest));
    dest.NRROUTE[0].ROUT_NEIGHBOUR = &routeA;
    dest.DEST_ROUTE = 1;

    if (is_active_route(&dest, &routeA)) {
        printf("PASS: found routeA via NRROUTE[0] with DEST_ROUTE=1\n");
    } else {
        printf("XFAIL: could not find routeA via NRROUTE[0] with DEST_ROUTE=1\n");
        printf("       (buggy code looked in INP3ROUTE[1] instead of NRROUTE[0])\n");
        failures++;
    }

    /* Test 2: NR route in slot 2, DEST_ROUTE=3 */
    memset(&dest, 0, sizeof(dest));
    dest.NRROUTE[2].ROUT_NEIGHBOUR = &routeB;
    dest.DEST_ROUTE = 3;

    if (is_active_route(&dest, &routeB)) {
        printf("PASS: found routeB via NRROUTE[2] with DEST_ROUTE=3\n");
    } else {
        printf("XFAIL: could not find routeB via NRROUTE[2] with DEST_ROUTE=3\n");
        printf("       (buggy code accessed INP3ROUTE[3] — out of bounds!)\n");
        failures++;
    }

    /* Test 3: INP3 route in slot 0, DEST_ROUTE=4 */
    memset(&dest, 0, sizeof(dest));
    dest.INP3ROUTE[0].ROUT_NEIGHBOUR = &routeA;
    dest.DEST_ROUTE = 4;

    if (is_active_route(&dest, &routeA)) {
        printf("PASS: found routeA via INP3ROUTE[0] with DEST_ROUTE=4\n");
    } else {
        printf("XFAIL: could not find routeA via INP3ROUTE[0] with DEST_ROUTE=4\n");
        printf("       (buggy code accessed INP3ROUTE[4] — out of bounds!)\n");
        failures++;
    }

    printf("\n%d test(s) failed\n", failures);
    return failures > 0 ? 1 : 0;
}
