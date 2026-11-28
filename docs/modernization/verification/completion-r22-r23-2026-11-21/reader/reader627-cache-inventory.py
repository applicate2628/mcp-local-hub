import os, stat, json, time, hashlib, tomllib, datetime, sys
from collections import deque
DISCOVERY=["<WORK_ROOT>\\mcp-product-rust-host-recovery-work-20261119","<WORK_ROOT>\\mcp-product-rust-members-ffi-20261118","<WORK_ROOT>\\mcp-product-rust-members-ffi-work-20261118","<WORK_ROOT>\\mcp-product-rust-native-members-20261118","<WORK_ROOT>\\mcp-product-rust-native-members-work-20261118","<WORK_ROOT>\\mcp-product-rust-root-wait-20261118","<WORK_ROOT>\\mcp-product-rust-root-wait-work-20261118","<WORK_ROOT>\\mcp-product-rust100-recovery-work-20261119","<WORK_ROOT>\\mcp-r23-rust-20261121","<WORK_ROOT>\\mcp-rust-receiving-followup-recovery-work-20261120","<WORK_ROOT>\\mcp-rust-supervisor-recovery-work-20261119","<WORK_ROOT>\\mcp-product-rust-members-ffi-20261118\\.scratch","<WORK_ROOT>\\mcp-product-rust-members-ffi-20261118\\.scratch\\member-ffi-20261118","<WORK_ROOT>\\mcp-product-rust-native-members-20261118\\.scratch","<WORK_ROOT>\\mcp-product-rust-native-members-20261118\\.scratch\\native-job-members-20261118","<WORK_ROOT>\\mcp-product-rust-root-wait-20261118\\.scratch","<WORK_ROOT>\\mcp-product-rust-root-wait-20261118\\.scratch\\root-wait-20261118","<WORK_ROOT>\\mcp-product-rust-members-ffi-work-20261118\\profile","<WORK_ROOT>\\mcp-product-rust-members-ffi-work-20261118\\appdata","<WORK_ROOT>\\mcp-product-rust-members-ffi-work-20261118\\localappdata","<WORK_ROOT>\\mcp-product-rust-native-members-work-20261118\\profile","<WORK_ROOT>\\mcp-product-rust-native-members-work-20261118\\profile\\AppData","<WORK_ROOT>\\mcp-product-rust-native-members-work-20261118\\appdata","<WORK_ROOT>\\mcp-product-rust-native-members-work-20261118\\localappdata","<WORK_ROOT>\\mcp-product-rust-root-wait-work-20261118\\profile","<WORK_ROOT>\\mcp-product-rust-root-wait-work-20261118\\appdata","<WORK_ROOT>\\mcp-product-rust-root-wait-work-20261118\\localappdata","<WORK_ROOT>\\mcp-product-rust-root-wait-work-20261118\\job-profile","<WORK_ROOT>\\mcp-product-rust-root-wait-work-20261118\\job-profile\\profile","<WORK_ROOT>\\mcp-product-rust-root-wait-work-20261118\\job-profile\\appdata","<WORK_ROOT>\\mcp-product-rust-root-wait-work-20261118\\job-profile\\localappdata"]
USER_CACHE=r'<USER_PROFILE>\.cargo\registry\cache'
LOCK=r'<WORK_ROOT>\mcp-r23-rust-20261121\source-pr627\experiments\rust-anchored-read\Cargo.lock'
NAMES={'archives','archive','vendor','.cargo','cargo-home','cargo_home','registry','cache'}
SKIP={'.git','target','targets','runtime','tmp','temp','trash','gocache','go-cache','gomodcache','node_modules','.venv'}
ENTRY_LIMIT, DEPTH_LIMIT, TIME_LIMIT=50000,32,30
start=time.monotonic()
out={'schema':2,'mode':'targeted existing Cargo storage inventory','observed_utc':datetime.datetime.now(datetime.timezone.utc).isoformat(),'cwd':os.getcwd(),'python':sys.executable,'limits':{'entries_per_storage':ENTRY_LIMIT,'directory_depth':DEPTH_LIMIT,'seconds_total':TIME_LIMIT},'discovery':[],'storages':[],'archives':[],'package_directories':[],'needed_candidates':[],'configuration':[],'tests_build_cargo_install_downloads':False,'remote_files_written':False,'global_absence_claimed':False}
def reparse(s):
    return bool(getattr(s,'st_file_attributes',0)&0x400) or stat.S_ISLNK(s.st_mode)
def checked(path):
    path=os.path.abspath(path)
    drive,tail=os.path.splitdrive(path)
    current=drive+os.sep
    for part in tail.strip('\\/').split(os.sep):
        current=os.path.join(current,part)
        s=os.lstat(current)
        if reparse(s): raise ValueError('reparse component '+current)
    return os.lstat(path)
s=checked(LOCK)
if s.st_size>100000: raise ValueError('lock size')
with open(LOCK,'rb') as f: raw=f.read()
blob=hashlib.sha1(b'blob '+str(len(raw)).encode()+b'\x00'+raw).hexdigest()
if blob!='bf195e14a3b725f5a64c8e577de56e2b605018a3': raise ValueError('lock identity')
packages=[p for p in tomllib.loads(raw.decode())['package'] if 'source' in p]
expected={p['name']+'-'+p['version']:p for p in packages}
needed={k for k,p in expected.items() if p['name'] not in {'windows-link','windows-sys'}}
out['lock']={'path':LOCK,'git_blob':blob,'sha256':hashlib.sha256(raw).hexdigest(),'needed':[{'name':expected[k]['name'],'version':expected[k]['version'],'checksum':expected[k]['checksum']} for k in sorted(needed)]}
found=[]
for path in DISCOVERY:
    record={'path':path,'entries':0,'storage_directories':[],'reparse_skipped':[],'complete':False}
    out['discovery'].append(record)
    try:
        if not stat.S_ISDIR(checked(path).st_mode): raise ValueError('not directory')
        with os.scandir(path) as it:
            for n,e in enumerate(it):
                if n>=1000:
                    record['limit_hit']='discovery entry cap'
                    break
                record['entries']+=1
                st=e.stat(follow_symlinks=False)
                if reparse(st):
                    record['reparse_skipped'].append(e.path)
                    continue
                if stat.S_ISDIR(st.st_mode) and e.name.lower() in NAMES:
                    record['storage_directories'].append(e.path)
                    found.append(e.path)
            else: record['complete']=True
    except Exception as e: record['error']=str(e)
found.append(USER_CACHE)
# Keep outer storage roots so nested registry/cache cannot duplicate traversal.
selected=[]
for path in sorted(set(found),key=lambda p:(len(p),p.lower())):
    if not any(os.path.commonpath([path,p]).lower()==p.lower() for p in selected if os.path.splitdrive(path)[0].lower()==os.path.splitdrive(p)[0].lower()):
        selected.append(path)
out['selected_storages']=selected
for root in selected:
    record={'path':root,'directories':0,'entries':0,'files':0,'reparse_skipped':0,'excluded_directories':0,'limits_hit':[],'errors':[],'complete':False}
    out['storages'].append(record)
    try:
        st=checked(root)
        if not stat.S_ISDIR(st.st_mode): raise ValueError('not directory')
    except FileNotFoundError as e:
        record['status']='not_found'
        record['detail']=str(e)
        continue
    except Exception as e:
        record['errors'].append(str(e))
        continue
    queue=deque([(root,0)])
    while queue:
        if time.monotonic()-start>TIME_LIMIT or record['entries']>=ENTRY_LIMIT:
            record['limits_hit'].append({'reason':'time' if time.monotonic()-start>TIME_LIMIT else 'entries','pending':len(queue)})
            break
        path,depth=queue.popleft()
        try:
            if reparse(os.lstat(path)):
                record['reparse_skipped']+=1
                continue
            with os.scandir(path) as it:
                for e in it:
                    if record['entries']>=ENTRY_LIMIT:
                        record['limits_hit'].append({'reason':'entries','path':path})
                        break
                    record['entries']+=1
                    st=e.stat(follow_symlinks=False)
                    if reparse(st):
                        record['reparse_skipped']+=1
                        continue
                    if stat.S_ISDIR(st.st_mode):
                        if e.name.lower() in SKIP:
                            record['excluded_directories']+=1
                        elif depth>=DEPTH_LIMIT:
                            record['limits_hit'].append({'reason':'depth','path':e.path})
                        else:
                            queue.append((e.path,depth+1))
                        if e.name in expected:
                            item={'path':e.path,'name':expected[e.name]['name'],'version':expected[e.name]['version'],'needed':e.name in needed,'qualified':False}
                            out['package_directories'].append(item)
                            if item['needed']: out['needed_candidates'].append(item)
                    elif stat.S_ISREG(st.st_mode):
                        record['files']+=1
                        low=e.name.lower()
                        if low.endswith('.crate'):
                            stem=e.name[:-6]
                            item={'path':e.path,'bytes':st.st_size,'lock_name_match':stem in expected,'needed':stem in needed}
                            if stem in expected:
                                item['expected_sha256']=expected[stem]['checksum']
                                if st.st_size<=32000000:
                                    with open(e.path,'rb') as f: item['sha256']=hashlib.file_digest(f,'sha256').hexdigest()
                                    item['lock_checksum_matches']=item['sha256']==item['expected_sha256']
                                else: item['hash_not_read']='archive size cap'
                            out['archives'].append(item)
                            if item['needed']: out['needed_candidates'].append(item)
                        if e.name=='config.toml' and st.st_size<100000:
                            with open(e.path,'rb') as f: config=f.read()
                            parsed=tomllib.loads(config.decode())
                            out['configuration'].append({'path':e.path,'sha256':hashlib.sha256(config).hexdigest(),'source':parsed.get('source'),'net':parsed.get('net')})
            record['directories']+=1
        except Exception as e: record['errors'].append({'path':path,'error':str(e)})
    record['complete']=not queue and not record['limits_hit'] and not record['errors']
    record['status']='complete' if record['complete'] else 'incomplete'
out['elapsed_seconds']=time.monotonic()-start
out['discovery_entries']=sum(r['entries'] for r in out['discovery'])
out['storage_entries']=sum(r['entries'] for r in out['storages'])
print('READER_CACHE_INVENTORY='+json.dumps(out,ensure_ascii=True),flush=True)

