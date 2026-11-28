import os, sys, stat, json, time, hashlib, tomllib, datetime
from collections import deque
BASE = r'<WORK_ROOT>'
ROOTS = ["mcp-product-rust-host-recovery-work-20261119","mcp-product-rust-members-ffi-20261118","mcp-product-rust-members-ffi-work-20261118","mcp-product-rust-native-members-20261118","mcp-product-rust-native-members-work-20261118","mcp-product-rust-root-wait-20261118","mcp-product-rust-root-wait-work-20261118","mcp-product-rust100-recovery-work-20261119","mcp-r23-rust-20261121","mcp-rust-receiving-followup-recovery-work-20261120","mcp-rust-supervisor-recovery-work-20261119"]
LOCK = BASE + r'\mcp-r23-rust-20261121\source-pr627\experiments\rust-anchored-read\Cargo.lock'
EXCLUDE = {'.git','target','targets','runtime','tmp','temp','trash','node_modules','gocache','gomodcache','.venv','venv'}
DEPTH, CAP, GLOBAL_CAP, SECONDS = 5, 3000, 25000, 25
start = time.monotonic()
out = {'schema':1,'mode':'read-only bounded filesystem inventory','observed_utc':datetime.datetime.now(datetime.timezone.utc).isoformat(),'python':sys.executable,'cwd':os.getcwd(),'limits':{'max_depth':DEPTH,'entries_per_root':CAP,'entries_total':GLOBAL_CAP,'seconds':SECONDS},'excluded_names':sorted(EXCLUDE),'tests_build_cargo_providers_run':False,'remote_files_written':False,'roots':[],'candidates':[],'archives':[]}
def reject(s):
    return bool(getattr(s,'st_file_attributes',0) & 0x400) or stat.S_ISLNK(s.st_mode)
def checked(path):
    path = os.path.abspath(path)
    drive, tail = os.path.splitdrive(path)
    current = drive + os.sep
    for part in tail.strip('\\/').split(os.sep):
        current = os.path.join(current,part)
        s = os.lstat(current)
        if reject(s):
            raise ValueError('reparse component: '+current)
    return os.lstat(path)
checked(BASE)
s = checked(LOCK)
if s.st_size > 100000:
    raise ValueError('unexpected lock size')
with open(LOCK,'rb') as f:
    raw = f.read()
blob = hashlib.sha1(b'blob '+str(len(raw)).encode()+b'\x00'+raw).hexdigest()
if blob != 'bf195e14a3b725f5a64c8e577de56e2b605018a3':
    raise ValueError('lock blob differs')
packages = tomllib.loads(raw.decode())['package']
needed = {(p['name'],p['version']):p['checksum'] for p in packages if 'source' in p and p['name'] not in {'windows-link','windows-sys'}}
out['lock'] = {'path':LOCK,'git_blob':blob,'sha256':hashlib.sha256(raw).hexdigest(),'missing_registry_packages':[{'name':n,'version':v,'checksum':h} for (n,v),h in needed.items()]}
archives = {n+'-'+v+'.crate':(n,v,h) for (n,v),h in needed.items()}
dirnames = {n+'-'+v:(n,v) for n,v in needed}
dirnames.update({n:(n,v) for n,v in needed})
total = 0
for name in ROOTS:
    root = os.path.join(BASE,name)
    report = {'path':root,'directories':0,'entries':0,'excluded_directories':0,'reparse_skipped':0,'depth_pruned':0,'limits_hit':[],'errors':[]}
    out['roots'].append(report)
    try:
        if not stat.S_ISDIR(checked(root).st_mode):
            raise ValueError('root is not directory')
    except Exception as e:
        report['errors'].append(str(e))
        continue
    queue = deque([(root,0)])
    while queue:
        if time.monotonic()-start >= SECONDS or total >= GLOBAL_CAP or report['entries'] >= CAP:
            report['limits_hit'].append({'reason':'time' if time.monotonic()-start>=SECONDS else 'entry_cap','pending_directories':len(queue)})
            break
        path, depth = queue.popleft()
        try:
            if reject(os.lstat(path)):
                report['reparse_skipped'] += 1
                continue
            with os.scandir(path) as it:
                entries=[]
                for entry in it:
                    if len(entries) >= min(CAP-report['entries'],GLOBAL_CAP-total):
                        report['limits_hit'].append({'reason':'directory_entry_cap','path':path})
                        break
                    entries.append(entry)
            entries.sort(key=lambda e:(0 if any(k in e.name.lower() for k in ('vendor','archive','cargo','registry','crate','cache')) else 1,e.name.lower()))
            report['directories'] += 1
        except Exception as e:
            report['errors'].append({'path':path,'error':str(e)})
            continue
        for entry in entries:
            report['entries']+=1
            total+=1
            low=entry.name.lower()
            try:
                st=entry.stat(follow_symlinks=False)
                if reject(st):
                    report['reparse_skipped']+=1
                    continue
                if stat.S_ISDIR(st.st_mode):
                    if low in EXCLUDE:
                        report['excluded_directories']+=1
                        continue
                    if entry.name in dirnames:
                        out['candidates'].append({'path':entry.path,'kind':'directory_name_candidate','expected_name':dirnames[entry.name][0],'expected_version':dirnames[entry.name][1],'qualified':False})
                    if depth < DEPTH:
                        queue.append((entry.path,depth+1))
                    else:
                        report['depth_pruned']+=1
                elif stat.S_ISREG(st.st_mode):
                    if low.endswith(('.crate','.zip','.tar','.tar.gz','.tgz','.tar.zst','.crate.gz.b64')):
                        out['archives'].append({'path':entry.path,'bytes':st.st_size})
                    if entry.name in archives:
                        n,v,want=archives[entry.name]
                        item={'path':entry.path,'kind':'crate_archive','name':n,'version':v,'expected_sha256':want,'bytes':st.st_size}
                        if st.st_size <= 32000000:
                            with open(entry.path,'rb') as f:
                                actual=hashlib.file_digest(f,'sha256').hexdigest()
                            item.update({'sha256':actual,'lock_checksum_matches':actual==want})
                        else:
                            item['hash_not_read']='size limit'
                        out['candidates'].append(item)
            except Exception as e:
                report['errors'].append({'path':entry.path,'error':str(e)})
out['entries_total']=total
out['elapsed_seconds']=time.monotonic()-start
out['global_absence_claimed']=False
print('READER_DEPENDENCY_INVENTORY='+json.dumps(out,ensure_ascii=True),flush=True)

