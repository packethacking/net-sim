/*
 * Test for Bug 4: sendAlltoOneNeigbour increments Route parameter
 * instead of continuing, corrupting the pointer for subsequent iterations.
 *
 * Compile:
 *   gcc -DBUGGY -o test_bug4_xfail test_bug4_route_corruption.c && ./test_bug4_xfail
 *   gcc         -o test_bug4_fixed test_bug4_route_corruption.c && ./test_bug4_fixed
 */

#include <string.h>
#include <stdio.h>

typedef unsigned char UCHAR;

struct ROUTE {
    UCHAR NEIGHBOUR_CALL[7];
    char label[16];
};

struct DEST_LIST {
    UCHAR DEST_CALL[7];
};

/* Simulates the loop body where the bug occurs */
static struct ROUTE *simulate_loop(struct ROUTE *Route,
                                    struct DEST_LIST *dests, int ndests) {
    int i;
    struct ROUTE *current = Route;

    for (i = 0; i < ndests; i++) {
        if (memcmp(current->NEIGHBOUR_CALL, dests[i].DEST_CALL, 7) == 0) {
#ifdef BUGGY
            current++;   /* BUG: corrupts Route pointer */
            continue;
#else
            continue;    /* FIX: just skip, don't touch Route */
#endif
        }
        /* After this point, 'current' should still be the original Route */
    }
    return current;
}

int main(void) {
    struct ROUTE routes[3];
    struct DEST_LIST dests[3];
    struct ROUTE *result;
    int failures = 0;

    memset(routes, 0, sizeof(routes));
    memset(dests, 0, sizeof(dests));

    /* Route we're sending to: "SIM1" */
    memcpy(routes[0].NEIGHBOUR_CALL, "SIM1\x00\x00\x00", 7);
    strcpy(routes[0].label, "Target");

    /* Adjacent route in memory (should NOT be used) */
    memcpy(routes[1].NEIGHBOUR_CALL, "SIM2\x00\x00\x00", 7);
    strcpy(routes[1].label, "Wrong!");

    /* Destinations: first matches Route's call, second doesn't */
    memcpy(dests[0].DEST_CALL, "SIM1\x00\x00\x00", 7);  /* self-match */
    memcpy(dests[1].DEST_CALL, "SIM3\x00\x00\x00", 7);  /* normal dest */

    result = simulate_loop(&routes[0], dests, 2);

    if (result == &routes[0]) {
        printf("PASS: Route pointer unchanged after self-match skip (%s)\n",
               result->label);
    } else {
        printf("XFAIL: Route pointer corrupted — now points to '%s' instead of 'Target'\n",
               result->label);
        printf("       (Route++ advanced to the next neighbour in memory)\n");
        failures++;
    }

    printf("\n%d test(s) failed\n", failures);
    return failures > 0 ? 1 : 0;
}
