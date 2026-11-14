/* Benign DLL-loader fixture, never a process-owner implementation.
 * Built by the qualification runner only. No I/O, thread creation, nested
 * LoadLibrary or process creation occurs in DllMain. Exports are deliberately
 * inert except version; tests exercise Open/Close, never process operations. */
#include <windows.h>
#include "owner_v1.h"
#ifndef OMIT_EXPORT
#define OMIT_EXPORT -1
#endif
#ifndef FIXTURE_VERSION
#define FIXTURE_VERSION 1u
#endif
#ifdef REQUIRE_DEPENDENCY
__declspec(dllimport) uint32_t mcphub_loader_fixture_dependency(void);
#endif
#define API __declspec(dllexport)
BOOL WINAPI DllMain(HINSTANCE module, DWORD reason, LPVOID reserved) {
    (void)module; (void)reserved;
#ifdef REFUSE_INITIALIZATION
    return reason != DLL_PROCESS_ATTACH;
#else
    (void)reason;
    return TRUE;
#endif
}
#if OMIT_EXPORT != 0
API uint32_t mh_version1(void) {
#ifdef REQUIRE_DEPENDENCY
    return mcphub_loader_fixture_dependency();
#else
    return FIXTURE_VERSION;
#endif
}
#endif
#if OMIT_EXPORT != 1
API uint32_t mh_create1(const mh_create *request,uint32_t size,mh_owner **owner,mh_out *out,uint32_t bytes){
    (void)request;(void)size;(void)owner;(void)out;(void)bytes;return MH_ARGUMENT;
}
#endif
#if OMIT_EXPORT != 2
API uint32_t mh_admit1(mh_owner *owner,uint32_t allow,mh_out *out,uint32_t bytes){
    (void)owner;(void)allow;(void)out;(void)bytes;return MH_ARGUMENT;
}
#endif
#if OMIT_EXPORT != 3
API uint32_t mh_stop1(mh_owner *owner,uint32_t code,mh_out *out,uint32_t bytes){
    (void)owner;(void)code;(void)out;(void)bytes;return MH_ARGUMENT;
}
#endif
#if OMIT_EXPORT != 4
API uint32_t mh_finish1(mh_owner *owner,uint32_t ms,mh_out *out,uint32_t bytes){
    (void)owner;(void)ms;(void)out;(void)bytes;return MH_ARGUMENT;
}
#endif
#if OMIT_EXPORT != 5
API uint32_t mh_release1(mh_owner **owner,mh_out *out,uint32_t bytes){
    (void)owner;(void)out;(void)bytes;return MH_ARGUMENT;
}
#endif
#if OMIT_EXPORT != 6
API uint32_t mh_admit_checked1(mh_owner *owner,mh_check_fn check,uintptr_t token,mh_out *out,uint32_t bytes){
    (void)owner;(void)check;(void)token;(void)out;(void)bytes;return MH_ARGUMENT;
}
#endif
#if OMIT_EXPORT != 7
API uint32_t mh_create_env1(const mh_create *request,uint32_t size,const mh_environment *env,uint32_t env_bytes,mh_owner **owner,mh_out *out,uint32_t bytes){
    (void)request;(void)size;(void)env;(void)env_bytes;(void)owner;(void)out;(void)bytes;return MH_ARGUMENT;
}
#endif
#if OMIT_EXPORT != 8
API uint32_t mh_create_io1(const mh_create *request,uint32_t size,const mh_io_context *context,uint32_t context_bytes,mh_owner **owner,mh_out *out,uint32_t bytes){
    (void)request;(void)size;(void)context;(void)context_bytes;(void)owner;(void)out;(void)bytes;return MH_ARGUMENT;
}
#endif
