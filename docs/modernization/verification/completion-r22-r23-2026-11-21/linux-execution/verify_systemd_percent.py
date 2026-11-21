"""Validate literal percent quoting with the installed parser, without service execution."""
import datetime
import hashlib
import json
from pathlib import Path
import shutil
import subprocess

ROOT = Path(__file__).resolve().parent / 'native-parser'
ROOT.mkdir()
exe = ROOT / 'release-%Z' / 'probe'
exe.parent.mkdir()
shutil.copyfile('/usr/bin/true', exe)
exe.chmod(0o700)
results = []
for mode, path in [('control', str(exe)), ('candidate', str(exe).replace('%', '%%'))]:
    unit = ROOT / (mode + '.service')
    # This isolates the ExecStart percent contract; it is not the full Product unit.
    unit.write_text('[Unit]\nDescription=Literal percent parser probe\nDefaultDependencies=no\n[Service]\nType=oneshot\nExecStart="' + path + '"\n')
    argv = ['/usr/bin/systemd-analyze', '--generators=no', '--man=no', 'verify', str(unit)]
    result = subprocess.run(argv, capture_output=True, timeout=30)
    (ROOT/(mode+'.stdout.log')).write_bytes(result.stdout)
    (ROOT/(mode+'.stderr.log')).write_bytes(result.stderr)
    results.append({'mode':mode,'args':argv[1:],'exit_code':result.returncode,
                    'unit_sha256':hashlib.sha256(unit.read_bytes()).hexdigest(),
                    'stdout_sha256':hashlib.sha256(result.stdout).hexdigest(),
                    'stderr_sha256':hashlib.sha256(result.stderr).hexdigest(),
                    'stderr':result.stderr.decode(errors='replace')})
receipt={'observed_utc':datetime.datetime.now(datetime.timezone.utc).isoformat(),
         'systemd_version':subprocess.run(['/usr/bin/systemd-analyze','--version'],capture_output=True,check=True).stdout.decode(),
         'service_registration':False,'service_start':False,'scope':'literal percent ExecStart parser contract',
         'results':results}
(ROOT/'receipt.json').write_text(json.dumps(receipt,indent=2)+'\n')
assert results[0]['exit_code'] != 0 and 'specifier' in results[0]['stderr'].lower(), results[0]
assert results[1]['exit_code'] == 0 and not results[1]['stderr'], results[1]
print(json.dumps({'control_exit':results[0]['exit_code'],'candidate_exit':results[1]['exit_code'],'matched':True}))
