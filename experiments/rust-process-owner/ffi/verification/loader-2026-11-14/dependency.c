/* Tiny owned dependency fixture; no side effects or process management. */
#include <windows.h>
#include <stdint.h>
BOOL WINAPI DllMain(HINSTANCE module,DWORD reason,LPVOID reserved) {
    (void)module;(void)reason;(void)reserved;return TRUE;
}
__declspec(dllexport) uint32_t mcphub_loader_fixture_dependency(void){return 1;}
