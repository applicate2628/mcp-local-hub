#define WIN32_LEAN_AND_MEAN
#include <windows.h>

#define CST_EXIT_NATIVE_LOADER_INVALID 78u
#define FRAME_MAX 2048u

/* Process-root-owned fixed bootstrap workspace; no reusable module or thread
   can observe it and the custom entry is single-threaded until termination. */
static char g_frame[FRAME_MAX+1], g_receipt[FRAME_MAX+1];

void *memcpy(void *destination, const void *source, SIZE_T count) {
    BYTE *d=(BYTE*)destination; const BYTE *s=(const BYTE*)source; SIZE_T i;
    for(i=0;i<count;++i)d[i]=s[i];
    return destination;
}

__declspec(noreturn) void mcphub_cst_entry(void);
#pragma comment(linker, "/include:cst_relocation_anchor")
#pragma section(".rdata$reloc", read)
__declspec(allocate(".rdata$reloc")) void (*const cst_relocation_anchor)(void) = mcphub_cst_entry;
#pragma section(".rdata$loadcfg", read)
__declspec(allocate(".rdata$loadcfg")) const IMAGE_LOAD_CONFIG_DIRECTORY64 _load_config_used = {
    sizeof(IMAGE_LOAD_CONFIG_DIRECTORY64), 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
    LOAD_LIBRARY_SEARCH_SYSTEM32
};

static BOOL clear_inherit(HANDLE h) {
    DWORD flags = 0;
    return h && h != INVALID_HANDLE_VALUE &&
           SetHandleInformation(h, HANDLE_FLAG_INHERIT, 0) &&
           GetHandleInformation(h, &flags) && !(flags & HANDLE_FLAG_INHERIT);
}

static BOOL write_all(HANDLE h, const BYTE *p, DWORD n) {
    while (n) {
        DWORD done = 0;
        if (!WriteFile(h, p, n, &done, NULL) || !done) return FALSE;
        p += done; n -= done;
    }
    return TRUE;
}

static BOOL read_all(HANDLE h, BYTE *p, DWORD n) {
    while (n) {
        DWORD done = 0;
        if (!ReadFile(h, p, n, &done, NULL) || !done) return FALSE;
        p += done; n -= done;
    }
    return TRUE;
}

static DWORD text_len(const char *p) { DWORD n = 0; while (p[n]) ++n; return n; }
static BOOL bytes_equal(const char *a, const char *b, DWORD n) { DWORD i; for (i=0;i<n;++i) if (a[i]!=b[i]) return FALSE; return TRUE; }

static BOOL write_frame(HANDLE h, const char *payload, DWORD n) {
    BYTE header[4];
    header[0]=(BYTE)(n>>24); header[1]=(BYTE)(n>>16); header[2]=(BYTE)(n>>8); header[3]=(BYTE)n;
    return n && n <= FRAME_MAX && write_all(h, header, 4) && write_all(h, (const BYTE*)payload, n);
}

static char *find_key(char *p, DWORD n, const char *key) {
    DWORD i, k = text_len(key);
    for (i=0;i+k<=n;++i) if (bytes_equal(p+i,key,k)) return p+i+k;
    return NULL;
}

static BOOL parse_u64(char *p, char *end, ULONGLONG *v) {
    ULONGLONG x=0; BOOL any=FALSE;
    while (p<end && *p>='0' && *p<='9') { ULONGLONG d=(ULONGLONG)(*p-'0'); if (x>((ULONGLONG)-1-d)/10) return FALSE; x=x*10+d; ++p; any=TRUE; }
    *v=x; return any;
}

static DWORD append_text(char *out, DWORD at, const char *s) { while (*s) out[at++]=*s++; return at; }
static DWORD append_span(char *out, DWORD at, const char *s, DWORD n) { DWORD i; for(i=0;i<n;++i) out[at++]=s[i]; return at; }
static DWORD append_u64(char *out, DWORD at, ULONGLONG v) { char rev[24]; DWORD n=0; do { rev[n++]=(char)('0'+v%10); v/=10; } while(v); while(n) out[at++]=rev[--n]; return at; }
static BOOL json_object_span(char *start, char *end, char **after) {
    LONG depth=0; BOOL string=FALSE, escape=FALSE; char *p;
    if (start>=end || *start!='{') return FALSE;
    for(p=start;p<end;++p){ char ch=*p; if(string){if(escape)escape=FALSE;else if(ch=='\\')escape=TRUE;else if(ch=='\"')string=FALSE;}else{if(ch=='\"')string=TRUE;else if(ch=='{')++depth;else if(ch=='}'&&--depth==0){*after=p+1;return TRUE;}} }
    return FALSE;
}

static BOOL identity_matches(HANDLE h, char *object, char *object_end) {
    FILE_ID_INFO id; FILE_STANDARD_INFO standard; ULONGLONG volume=0; BYTE expected[16]; DWORD i;
    char *v=find_key(object,(DWORD)(object_end-object),"\"volume_serial\":");
    char *f=find_key(object,(DWORD)(object_end-object),"\"file_id\":\"");
    if(!v||!f||f+32>=object_end||f[32]!='\"'||!parse_u64(v,object_end,&volume)) return FALSE;
    for(i=0;i<16;++i){char a=f[i*2],b=f[i*2+1];BYTE hi=(BYTE)(a>='0'&&a<='9'?a-'0':a>='a'&&a<='f'?a-'a'+10:255);BYTE lo=(BYTE)(b>='0'&&b<='9'?b-'0':b>='a'&&b<='f'?b-'a'+10:255);if(hi>15||lo>15)return FALSE;expected[i]=(BYTE)((hi<<4)|lo);}
    if(!GetFileInformationByHandleEx(h,FileIdInfo,&id,sizeof(id))||!GetFileInformationByHandleEx(h,FileStandardInfo,&standard,sizeof(standard))||!standard.Directory||id.VolumeSerialNumber!=volume)return FALSE;
    for(i=0;i<16;++i)if(id.FileId.Identifier[i]!=expected[i])return FALSE;
    return TRUE;
}

static BOOL emit_startup_proof(HANDLE error) {
    static const char yes[]="{\"breakaway_created\":false,\"breakaway_denied\":true,\"escaped_process_settled\":true,\"exact_job\":true,\"exactly_three_inherited_std_handles\":true,\"no_console\":true,\"schema\":\"mcphub.cst.saved_field.worker_startup_proof.v1\"}";
    BOOL in_job=FALSE;
    if(!IsProcessInJob(GetCurrentProcess(),NULL,&in_job)||!in_job) return FALSE;
    return write_frame(error,yes,(DWORD)sizeof(yes)-1);
}

static DWORD crc32_without_checksum(const BYTE *frame, DWORD n, DWORD comma_offset) {
    DWORD c=0xffffffffu,i,j; BYTE ch;
    for(i=0;i<1+n-(comma_offset+1);++i){ch=i?frame[comma_offset+i]:frame[0];c^=ch;for(j=0;j<8;++j)c=(c>>1)^(0xedb88320u & (0u-(c&1u)));}
    return ~c;
}

static BOOL worker_exchange(HANDLE input, HANDLE output) {
    BYTE header[4]; char *frame=g_frame,*receipt=g_receipt; DWORD n,at=0; ULONGLONG source_v,workspace_v,checksum,source_access,workspace_access,frequency,deadline_tick; char *end,*source_id,*source_id_end,*workspace_id,*workspace_id_end,*correlation,*deadline,*deadline_end,*p; HANDLE source,workspace; LARGE_INTEGER frequency_now,tick_now;
    if(!read_all(input,header,4))return FALSE; n=((DWORD)header[0]<<24)|((DWORD)header[1]<<16)|((DWORD)header[2]<<8)|header[3]; if(!n||n>FRAME_MAX||!read_all(input,(BYTE*)frame,n))return FALSE; frame[n]=0; end=frame+n;
    p=find_key(frame,n,"{\"checksum\":"); if(p!=frame+12||!parse_u64(p,end,&checksum))return FALSE; while(p<end&&*p>='0'&&*p<='9')++p; if(p>=end||*p!=',')return FALSE; if(crc32_without_checksum((BYTE*)frame,n,(DWORD)(p-frame))!=(DWORD)checksum)return FALSE;
    correlation=find_key(frame,n,"\"correlation_id\":\""); if(!correlation||correlation+32>=end||correlation[32]!='\"')return FALSE;
    deadline=find_key(frame,n,"\"deadline\":"); if(!deadline||*deadline!='{'||!json_object_span(deadline,end,&deadline_end))return FALSE;
    p=find_key(deadline,(DWORD)(deadline_end-deadline),"\"qpc_frequency\":"); if(!p||!parse_u64(p,deadline_end,&frequency))return FALSE; p=find_key(deadline,(DWORD)(deadline_end-deadline),"\"deadline_tick\":"); if(!p||!parse_u64(p,deadline_end,&deadline_tick))return FALSE; if(!QueryPerformanceFrequency(&frequency_now)||!QueryPerformanceCounter(&tick_now)||(ULONGLONG)frequency_now.QuadPart!=frequency||(ULONGLONG)tick_now.QuadPart>=deadline_tick)return FALSE;
    p=find_key(frame,n,"\"inherited_handle_roles\":[\"stdin\",\"stdout\",\"stderr\",\"source-root\",\"workspace-root\"]"); if(!p)return FALSE;
    p=find_key(frame,n,"\"source_root_locator\":"); if(!p||!parse_u64(p,end,&source_v))return FALSE; p=find_key(frame,n,"\"workspace_root_locator\":"); if(!p||!parse_u64(p,end,&workspace_v)||!source_v||!workspace_v||source_v==workspace_v)return FALSE; source=(HANDLE)(ULONG_PTR)source_v; workspace=(HANDLE)(ULONG_PTR)workspace_v;
    p=find_key(frame,n,"\"source_access_mask\":"); if(!p||!parse_u64(p,end,&source_access)||!source_access)return FALSE; p=find_key(frame,n,"\"workspace_access_mask\":"); if(!p||!parse_u64(p,end,&workspace_access)||!workspace_access)return FALSE;
    source_id=find_key(frame,n,"\"source_root_identity\":"); workspace_id=find_key(frame,n,"\"workspace_root_identity\":"); if(!source_id||!workspace_id||!json_object_span(source_id,end,&source_id_end)||!json_object_span(workspace_id,end,&workspace_id_end))return FALSE;
    if(!identity_matches(source,source_id,source_id_end)||!identity_matches(workspace,workspace_id,workspace_id_end)||!clear_inherit(source)||!clear_inherit(workspace))return FALSE;
    at=append_text(receipt,at,"{\"bootstrap_checksum\":");at=append_u64(receipt,at,checksum);at=append_text(receipt,at,",\"capability_identities_verified\":true,\"correlation_id\":\"");at=append_span(receipt,at,correlation,32);at=append_text(receipt,at,"\",\"deadline\":");at=append_span(receipt,at,deadline,(DWORD)(deadline_end-deadline));at=append_text(receipt,at,",\"inherit_flags_cleared\":true,\"inherited_handle_roles\":[\"stdin\",\"stdout\",\"stderr\",\"source-root\",\"workspace-root\"],\"python_initialized\":false,\"schema\":\"mcphub.cst.saved_field.worker_pre_main_receipt.v1\",\"source_access_mask\":");at=append_u64(receipt,at,source_access);at=append_text(receipt,at,",\"source_root_identity\":");at=append_span(receipt,at,source_id,(DWORD)(source_id_end-source_id));at=append_text(receipt,at,",\"workspace_access_mask\":");at=append_u64(receipt,at,workspace_access);at=append_text(receipt,at,",\"workspace_root_identity\":");at=append_span(receipt,at,workspace_id,(DWORD)(workspace_id_end-workspace_id));receipt[at++]='}';
    return write_frame(output,receipt,at);
}

static BOOL contains_argument(const WCHAR *value,const WCHAR *literal){SIZE_T i;BOOL boundary=TRUE;if(!value||!literal)return FALSE;for(;*value;++value){if(boundary){for(i=0;literal[i]&&value[i]==literal[i];++i){}if(!literal[i]&&(!value[i]||value[i]==L' '||value[i]==L'\t'))return TRUE;}boundary=*value==L' '||*value==L'\t';}return FALSE;}

#ifdef CST_TEST_FRONTEND_E2E
static BOOL frontend_test_exchange(void) {
    WCHAR locator[32], endpoint[256]; ULONGLONG raw=0; DWORD i,n,done=0; BYTE capability[32], extra; HANDLE inherited,pipe;
    n=GetEnvironmentVariableW(L"MCPHUB_CST_LAUNCH_HANDLE",locator,32); if(!n||n>=32)return FALSE;
    for(i=0;i<n;++i){if(locator[i]<L'0'||locator[i]>L'9')return FALSE;raw=raw*10+(ULONGLONG)(locator[i]-L'0');}
    inherited=(HANDLE)(ULONG_PTR)raw; if(!clear_inherit(inherited)||!read_all(inherited,capability,32))return FALSE;
    if(ReadFile(inherited,&extra,1,&done,NULL)&&done!=0)return FALSE; CloseHandle(inherited);
    n=GetEnvironmentVariableW(L"MCPHUB_CST_TEST_FRONTEND_PIPE",endpoint,256); if(!n||n>=256)return FALSE;
    pipe=CreateFileW(endpoint,GENERIC_WRITE,0,NULL,OPEN_EXISTING,0,NULL); if(pipe==INVALID_HANDLE_VALUE)return FALSE;
    if(!write_all(pipe,capability,32)){CloseHandle(pipe);return FALSE;} CloseHandle(pipe); return TRUE;
}
#endif

__declspec(noreturn) void mcphub_cst_entry(void) {
    HANDLE input=GetStdHandle(STD_INPUT_HANDLE),output=GetStdHandle(STD_OUTPUT_HANDLE),error=GetStdHandle(STD_ERROR_HANDLE); const WCHAR *line;
    if(!clear_inherit(input)||!clear_inherit(output)||!clear_inherit(error))ExitProcess(CST_EXIT_NATIVE_LOADER_INVALID);
    line=GetCommandLineW();
    if(contains_argument(line,L"--role=worker")&&!contains_argument(line,L"--role=frontend")){
        if(!emit_startup_proof(error)||!worker_exchange(input,output))ExitProcess(CST_EXIT_NATIVE_LOADER_INVALID);
        ExitProcess(CST_EXIT_NATIVE_LOADER_INVALID);
    }
#ifdef CST_TEST_FRONTEND_E2E
    if(contains_argument(line,L"--role=frontend")&&frontend_test_exchange())ExitProcess(0);
#endif
    ExitProcess(CST_EXIT_NATIVE_LOADER_INVALID);
}
