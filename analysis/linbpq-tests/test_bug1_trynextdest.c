/*
 * Test for Bug 1: L3TRYNEXTDEST route wraparound uses = instead of ==.
 *
 * Compile:
 *   gcc -DBUGGY -o test_bug1_xfail test_bug1_trynextdest.c && ./test_bug1_xfail
 *   gcc         -o test_bug1_fixed test_bug1_trynextdest.c && ./test_bug1_fixed
 *
 * With -DBUGGY: all tests XFAIL (shows the bug)
 * Without:      all tests PASS  (shows the fix)
 */

#include <string.h>
#include <stdio.h>

typedef unsigned char UCHAR;

struct ROUTE_STUB {
    char name[8];
};

struct NR_DEST_ROUTE_ENTRY {
    struct ROUTE_STUB *ROUT_NEIGHBOUR;
    UCHAR ROUT_QUALITY;
    UCHAR ROUT_OBSCOUNT;
    UCHAR ROUT_LOCKED;
};

struct INP3_DEST_ROUTE_ENTRY {
    struct ROUTE_STUB *ROUT_NEIGHBOUR;
    unsigned short STT;
    UCHAR Hops;
};

struct DEST_LIST {
    UCHAR DEST_ROUTE;
    struct NR_DEST_ROUTE_ENTRY NRROUTE[3];
    struct INP3_DEST_ROUTE_ENTRY INP3ROUTE[3];
};

static void trynextdest_wrap(struct DEST_LIST *DEST) {
    DEST->DEST_ROUTE++;
#ifdef BUGGY
    if (DEST->DEST_ROUTE = 7)
#else
    if (DEST->DEST_ROUTE == 7)
#endif
        DEST->DEST_ROUTE = 1;
}

static int test(const char *label, int initial, int expected) {
    struct DEST_LIST dest;
    memset(&dest, 0, sizeof(dest));
    dest.DEST_ROUTE = initial;
    trynextdest_wrap(&dest);
    if (dest.DEST_ROUTE == expected) {
        printf("PASS: %s (route %d -> %d)\n", label, initial, dest.DEST_ROUTE);
        return 0;
    }
    printf("XFAIL: %s (route %d -> %d, expected %d)\n",
           label, initial, dest.DEST_ROUTE, expected);
    return 1;
}

int main(void) {
    int failures = 0;

    failures += test("NR route 1 advances to 2",   1, 2);
    failures += test("NR route 2 advances to 3",   2, 3);
    failures += test("NR route 3 advances to 4",   3, 4);
    failures += test("INP3 route 4 advances to 5", 4, 5);
    failures += test("INP3 route 5 advances to 6", 5, 6);
    failures += test("INP3 route 6 wraps to 1",    6, 1);

    printf("\n%d test(s) failed\n", failures);
    return failures > 0 ? 1 : 0;
}
