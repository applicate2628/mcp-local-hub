"""Figure 1: actual scoped retention observations; no modelled values."""
from pathlib import Path
import hashlib
import json
import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt
import numpy as np

root = Path(__file__).resolve().parent
source = root / 'retention-result.json'
assert hashlib.sha256(source.read_bytes()).hexdigest() == '9a29e3d73baa7188b1e3ee8d280cb596548c544ce5c15f3bc2adaec72da0afab'
data = json.loads(source.read_text())
assert data['validated'] is True and not data['problems']
cohorts = sorted(data['cohort_descriptive_statistics'], key=lambda c: (c['clients'], ['selected-32','selected-1000','last-marker'].index(c['case'])))
plt.rcParams.update({'font.family':'DejaVu Sans','font.size':11,'svg.fonttype':'none','axes.spines.top':False,'axes.spines.right':False,'axes.spines.left':False})
fig, ax = plt.subplots(figsize=(12, 8.4))
fig.subplots_adjust(left=.25,right=.95,top=.83,bottom=.24)
y=np.arange(len(cohorts))
metric='results_and_wire_heap_alloc_delta_bytes'
for variant, offset, color, label in [('before-clear',-.17,'#7b8798','До очистки ссылок'),('current',.17,'#087f8c','Текущий код')]:
    med=np.array([c[variant][metric]['median'] for c in cohorts])/1048576
    low=np.array([c[variant][metric]['minimum'] for c in cohorts])/1048576
    high=np.array([c[variant][metric]['maximum'] for c in cohorts])/1048576
    ax.barh(y+offset,med,height=.29,color=color,label=label)
    ax.errorbar(med,y+offset,xerr=[med-low,high-med],fmt='none',ecolor='#172b3a',capsize=3,lw=1)
    for yy,m in zip(y+offset,med):
        ax.text(m+.35,yy,f'{m:.3f}',va='center',ha='left',fontsize=10,color='#172b3a')
ax.set_yticks(y,[f"{c['case']}  ·  {'1 работник' if c['clients']==1 else '4 работника'}" for c in cohorts])
ax.set_ylim(len(cohorts)-.5,-.6)
ax.set_xlim(0,50)
ax.set_xticks(range(0,51,10))
ax.set_xlabel('Прирост HeapAlloc относительно собственного baseline, МиБ')
ax.xaxis.grid(True,color='#dce3e8',lw=.7)
ax.set_axisbelow(True)
ax.tick_params(axis='y',length=0,pad=12)
ax.axhline(2.5,color='#b9c7d0',lw=.8)
ax.legend(loc='lower left',bbox_to_anchor=(-.01,1.02),ncol=2,frameon=False)
fig.suptitle('CMake: удержание результатов и закодированных тел',x=.05,y=.96,ha='left',fontsize=19,fontweight='bold',color='#172b3a')
fig.text(.05,.91,'Рисунок 1. Медианы и диапазоны трёх наблюдений на сценарий; полезный результат совпал.',fontsize=11,color='#475569')
fig.text(.05,.075,'Память всего Go-процесса после двух принудительных сборок мусора. 1 МиБ = 1 048 576 байт.\nНастоящий CMake создал исходную трассу на синтетическом проекте R22; производственный бюджет не установлен.\nWindows/amd64 · Go 1.27.4 · GOMAXPROCS=4 · точная база #624: 81dcf67d…',fontsize=10,color='#475569',linespacing=1.6)
for extension in ('svg','png'):
    fig.savefig(root / f'retention-figure.{extension}',dpi=180,facecolor='white',metadata={'Title':'R25 CMake retained Go heap: measured medians and ranges'})
print('Saved retention-figure.svg and retention-figure.png from pinned verified observations.')
