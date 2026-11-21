"""Run pinned Linux regression binaries; never register or start services."""
import collections
import datetime
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import subprocess
import time

ROOT = Path(__file__).resolve().parent
PIN = {
    'control': '34304a1abcc27a252c266e09122a2c5646da517c9814a132dcf07635767d6471',
    'candidate': '7909a802fa287d3460127ed328c6f22a50cf2552af0acd9a81f6e61a7a3e8182',
}
SELECT = '^(TestRenderLinuxUnit_PreservesLiteralPercent|TestLinuxBackend_PercentPathRoundTripAndLegacyDrift)$'
ADJACENT = '^(TestRenderLinuxUnit_QuotesExecStartPathWithSpaces|TestLinuxBackend_EnableWritesUnit|TestLinuxBackend_EnableStrictMode|TestLinuxBackend_StatusSnapshotSpecFingerprintTracksUnitAndEnabledButNotLiveness)$'

def digest(raw):
    return hashlib.sha256(raw).hexdigest()

def run(mode, label, count, select, expected):
    binary = ROOT / (mode + '.test')
    assert digest(binary.read_bytes()) == PIN[mode]
    out = ROOT / label
    out.mkdir()
    temp = out / 'tmp'
    temp.mkdir()
    env = os.environ.copy()
    env.update(TMPDIR=str(temp), TEMP=str(temp), TMP=str(temp), GOMAXPROCS='2', GOTELEMETRY='off')
    argv = [str(binary), '-test.v', '-test.run=' + select, '-test.count=' + str(count), '-test.timeout=30s']
    receipt = {'label':label, 'binary_sha256':PIN[mode], 'args':argv[1:], 'platform':platform.platform(),
               'started_utc':datetime.datetime.now(datetime.timezone.utc).isoformat(), 'direct_timeout_seconds':45,
               'source_commit':'ba17867125b3728fb734e90b383639a3ff9175ee', 'format':'Go verbose test protocol; no package JSON events',
               'native_service_operations':False, 'race_detector':False}
    (out / 'started.json').write_text(json.dumps(receipt,indent=2)+'\n')
    started = time.monotonic()
    child = None
    try:
        with (out/'stdout.log').open('xb') as stdout, (out/'stderr.log').open('xb') as stderr:
            child = subprocess.Popen(argv, cwd=ROOT, env=env, stdin=subprocess.DEVNULL, stdout=stdout, stderr=stderr)
            receipt['pid'] = child.pid
            receipt['exit_code'] = child.wait(timeout=45)
            receipt['settled'] = True
    except BaseException as error:
        receipt['error'] = type(error).__name__ + ': ' + str(error)
        if child is not None and child.poll() is None:
            child.terminate()
            receipt['exit_code'] = child.wait(timeout=10)
            receipt['settled'] = True
        raise
    finally:
        receipt['seconds'] = time.monotonic()-started
        for stream in ('stdout','stderr'):
            raw=(out/(stream+'.log')).read_bytes()
            receipt[stream]={'bytes':len(raw),'sha256':digest(raw)}
        (out/'receipt.json').write_text(json.dumps(receipt,indent=2)+'\n')
    raw=(out/'stdout.log').read_text()
    events = re.findall(r'^\s*--- (PASS|FAIL|SKIP): (\S+)',raw,re.M)
    counts=collections.Counter(a for a,n in events)
    assert receipt['exit_code'] == expected[0], receipt
    assert counts == expected[1], counts
    if mode == 'control':
        assert raw.count('SYSTEMD-PERCENT:') == 10
        assert raw.count('SYSTEMD-PERCENT-WRITE:') == 1
    receipt.update(test_events=[{'action':a,'name':n} for a,n in events], oracle_matched=True)
    (out/'receipt.json').write_text(json.dumps(receipt,indent=2)+'\n')
    print(json.dumps({'label':label,'exit':receipt['exit_code'],'counts':counts,'oracle_matched':True}))

if __name__ == '__main__':
    assert platform.system() == 'Linux' and platform.machine() == 'x86_64'
    run('control','control-run',1,SELECT,(1,{'PASS':2,'FAIL':12}))
    run('candidate','candidate-run',2,SELECT,(0,{'PASS':28}))
    run('candidate','adjacent-run',1,ADJACENT,(0,{'PASS':4}))
