/*
 * Test for Bug 5: RouteLastTT indexed by source neighbour instead of
 * destination neighbour in sendAlltoOneNeigbour and SendRIFToNewNeighbour.
 *
 * Compile:
 *   gcc -DBUGGY -o test_bug5_xfail test_bug5_routelasttt_index.c && ./test_bug5_xfail
 *   gcc         -o test_bug5_fixed test_bug5_routelasttt_index.c && ./test_bug5_fixed
 */

#include <string.h>
#include <stdio.h>

typedef unsigned short uint16_t;

struct ROUTE {
    int recNum;
    char name[8];
};

struct INP3_DEST_ROUTE_ENTRY {
    struct ROUTE *ROUT_NEIGHBOUR;
    unsigned short STT;
    unsigned char Hops;
};

#define MAX_NEIGHBOURS 8

struct DEST_LIST {
    struct INP3_DEST_ROUTE_ENTRY INP3ROUTE[3];
    uint16_t RouteLastTT[MAX_NEIGHBOURS];
};

/* Simulates the RouteLastTT update logic from sendAlltoOneNeigbour */
static void update_lasttt(struct DEST_LIST *Dest,
                           struct ROUTE *Route,              /* who we send to */
                           struct INP3_DEST_ROUTE_ENTRY *Entry, /* best route */
                           int sendTT) {
#ifdef BUGGY
    /* BUG: uses source neighbour's recNum */
    Dest->RouteLastTT[Entry->ROUT_NEIGHBOUR->recNum] = sendTT;
#else
    /* FIX: uses destination neighbour's recNum */
    Dest->RouteLastTT[Route->recNum] = sendTT;
#endif
}

int main(void) {
    struct DEST_LIST dest;
    struct ROUTE sourceNeighbour = { .recNum = 2, .name = "SrcNbr" };
    struct ROUTE destNeighbour   = { .recNum = 5, .name = "DstNbr" };
    struct INP3_DEST_ROUTE_ENTRY entry;
    int failures = 0;

    memset(&dest, 0, sizeof(dest));

    entry.ROUT_NEIGHBOUR = &sourceNeighbour;
    entry.STT = 100;
    entry.Hops = 2;

    update_lasttt(&dest, &destNeighbour, &entry, 150);

    /* Check: RouteLastTT[5] (dest) should be 150 */
    if (dest.RouteLastTT[destNeighbour.recNum] == 150) {
        printf("PASS: RouteLastTT[dest=%d] = %d (correctly tracks dest neighbour)\n",
               destNeighbour.recNum, dest.RouteLastTT[destNeighbour.recNum]);
    } else {
        printf("XFAIL: RouteLastTT[dest=%d] = %d (expected 150, not updated)\n",
               destNeighbour.recNum, dest.RouteLastTT[destNeighbour.recNum]);
        failures++;
    }

    /* Check: RouteLastTT[2] (source) should be 0 (untouched) */
    if (dest.RouteLastTT[sourceNeighbour.recNum] == 0) {
        printf("PASS: RouteLastTT[src=%d] = %d (correctly untouched)\n",
               sourceNeighbour.recNum, dest.RouteLastTT[sourceNeighbour.recNum]);
    } else {
        printf("XFAIL: RouteLastTT[src=%d] = %d (should be 0, was written to by mistake)\n",
               sourceNeighbour.recNum, dest.RouteLastTT[sourceNeighbour.recNum]);
        failures++;
    }

    printf("\n%d test(s) failed\n", failures);
    return failures > 0 ? 1 : 0;
}
