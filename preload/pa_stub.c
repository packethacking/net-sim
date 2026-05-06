/*
 * Stub for libportaudio's Pa_Initialize / Pa_Terminate.
 *
 * samoyed unconditionally calls Pa_Initialize at startup even when ADEVICE
 * uses stdin / UDP and no portaudio stream will ever be opened. On a
 * headless host with no PulseAudio daemon (which we deliberately avoid per
 * the simulator's hard rules) Pa_Initialize fails fatally.
 *
 * LD_PRELOAD this shim in front of every samoyed-direwolf the router
 * spawns. samoyed never calls any other Pa_* functions on the stdin/UDP
 * path, so a 6-line shim is sufficient.
 */
typedef int PaError;
PaError Pa_Initialize(void) { return 0; }
PaError Pa_Terminate(void)  { return 0; }
