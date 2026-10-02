#!/usr/bin/env python3
"""Render the scored evidence as a self-contained, readable report."""
import html
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parent

def read(name):
    return json.loads((ROOT / name).read_text())


def esc(value):
    return html.escape(str(value))


def link(url, title):
    return f'<a href="{esc(url)}">{esc(title)}</a>'


def main():
    manifest = read('manifest.json')
    scores = read('scores.json')
    rubric = read('historical-rubric.json')
    judgments = read('judgments.json')
    lookup = {(j['case'], j['arm'], j['index']): j for j in judgments}
    root_url = 'https://github.com/QuesmaOrg/quesma-shipper'
    parts = ['''<!doctype html><html lang="en"><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Review skill backtest — PR 85</title>
<style>
body{font:16px/1.55 system-ui,sans-serif;color:#1d2835;background:#fff;max-width:1080px;margin:auto;padding:36px 24px}
h1{font-size:2rem;line-height:1.2}h2{margin-top:2rem}h3{margin-top:1.6rem}a{color:#125da8}
table{width:100%;border-collapse:collapse;margin:18px 0;font-size:14px}th,td{padding:10px;text-align:left;border-bottom:1px solid #ced7df;vertical-align:top}th{background:#f0f4f7}
.note{background:#f2f5f8;border-left:4px solid #55718a;padding:14px 18px}.small{font-size:14px;color:#4b5968}
code{font-size:.9em;overflow-wrap:anywhere}details{border:1px solid #d4dce3;border-radius:5px;padding:12px;margin:10px 0}summary{cursor:pointer;font-weight:600}li{margin:7px 0}
@media(max-width:650px){body{padding:18px 12px}table{font-size:12px}th,td{padding:6px}}
@media print{details{break-inside:avoid}body{max-width:none;font-size:11pt}a{color:inherit}}
</style><body>
<p class="small">Quesma Shipper · retrospective evaluation · 2 October 2026</p>
<h1>The review skills improved coverage in this backtest, but did not replace generic review</h1>
<p>Twelve reviews covered three Fleet Manager PRs and three shipper PRs. Skill-guided reviews produced
<strong>9 supported findings</strong>; generic reviews produced <strong>6 supported findings and 1 false positive</strong>.
Five supported findings were unique to the skills, and two were unique to generic review. Combining both yielded 11 distinct supported issues.</p>
<p class="note"><strong>This is a replay of training examples, not validation on unseen PRs.</strong>
Every selected PR is explicitly referenced in the skills. This shows useful recovery of known failure patterns,
not an unbiased estimate of future performance. One run per arm per PR, a small purposive sample,
and coordinator adjudication do not support statistical significance or a general precision claim.</p>
<h2>Paired results</h2>
<p>Counts below are supported, independently actionable findings after adjudication, including concrete compatibility and documentation issues.
They are not raw comment counts. Priorities were preserved but not scored.</p>
<table><thead><tr><th>Component / PR</th><th>Change</th><th>Generic</th><th>Skill</th><th>Comparison</th></tr></thead><tbody>''']
    comparisons = {
        'f1': 'Both: CSV expands beyond request limit. Skill also: stale mixed-replica upgrade runbook.',
        'f2': 'Both clean. Focused tests passed; no unsupported findings.',
        'f3': 'Both: rerunning with :latest need not upgrade; explicit empty telemetry URL restores old value. Generic additionally raised one rejected P1.',
        's1': 'Skill: both historical lost-content cases. Generic: removed source ID rejects existing configuration.',
        's2': 'Both: process stderr misses managed log. Skill also: mixed-install removal and multiline home paths. Generic also: switched-away network-user agents missed by removal.',
        's3': 'Both clean. Intended deny enforcement remained intact.',
    }
    for case in manifest:
        row = next(x for x in scores['cases'] if x['case'] == case['case'])
        parts.append('<tr><td>' + esc(case['component']) + ' ' + link(f"{root_url}/pull/{case['pr']}", f"#{case['pr']}") + '</td><td>' + esc(case['title']) + '</td><td>' + str(row['generic']['supported']) + (' + 1 false positive' if row['generic']['false_positive'] else '') + '</td><td>' + str(row['skill']['supported']) + '</td><td>' + esc(comparisons[case['case']]) + '</td></tr>')
    parts.append('''</tbody></table>
<p>Fleet Manager: <strong>4 vs 3</strong> supported findings in favor of skills. Shipper: <strong>5 vs 3</strong>.
These counts include one skill-only runbook defect and one generic-only compatibility risk; they should not be described as nine versus six reproduced runtime bugs.</p>
<h2>What the historical reviews tell us</h2>
<p>The frozen rubric contains eight behavioral issues, one test-isolation issue and one operator-documentation issue.
Skills recovered <strong>5/10</strong> and generic reviews <strong>0/10 under exact-root-cause matching</strong>.
The generic macOS review found a related logging defect at final error reporting, rather than the historical validation-before-log ordering;
counting that broader symptom as a match gives generic <strong>1/10</strong>. Skills recovered 5/8 historical behavioral issues.</p>
<p>Neither arm recovered the five separately tracked maintainability items. The runs therefore provide no evidence of improved code-cleanliness coverage.
Both missed the historical age-key custody guidance request. The skill review also missed the macOS enrollment retry,
error-cause preservation, doctor remedy and host-dependent-test issues.</p>
<p>The managed enrollment retry depends on a control-plane implementation absent from the selected macOS snapshot.
Keeping it in the frozen denominator makes the input limitation visible; excluding it changes overall recovery to 5/9 versus 0/9
(or 1/9 with the broad logging match). No denominator was expanded to reward newly discovered findings.</p>
<table><thead><tr><th>Historical item</th><th>Type</th><th>Generic exact match</th><th>Skill exact match</th></tr></thead><tbody>''')
    for item in rubric['items']:
        found = {a: any(item['id'] in j['historical'] for j in judgments if j['arm'] == a) for a in ('generic', 'skill')}
        parts.append('<tr><td>' + link(item['source'], item['title']) + '</td><td>' + esc(item['kind']) + '</td><td>' + ('Yes' if found['generic'] else 'No') + '</td><td>' + ('Yes' if found['skill'] else 'No') + '</td></tr>')
    parts.append('''</tbody></table>
<p>#65's revoked-install ambiguity was excluded: follow-up preserved that behavior deliberately and clarified the message.
A later #53 hidden-command complaint was disproven, and a later stale-grant regression was absent from the selected snapshot.
#67 and #68 had no substantive historical findings; that is absence of labels, not proof that they are bug-free.</p>
<h2>Method and limits</h2>
<ul>
<li>Skills pinned to PR #85 commit <code>7eb78e6a4bd8ae07b8858bccaff89dc7dd597726</code>. Tested the three files per component, without editing them.</li>
<li>Selected varied change areas and excluded #64 and #79, which PR #85 explicitly used for tuning. This does not remove training contamination: all six remaining PRs are also cited.</li>
<li>Used the earliest inline-reviewed revision retained in each PR's history, or the single-commit head when there were no inline comments. Bases are merge-bases with GitHub's recorded base SHA. This tests pre-fix behavior rather than already-fixed merge results.</li>
<li>Each pair had separate reviewer contexts, identical source snapshots, the same generic review prompt and repository instructions. Only the skill arm received the extra skill files. No model/effort overrides. The exact backend model version and token usage were not exposed or recorded.</li>
<li>Eleven reviewer contexts produced twelve runs: the #67 skill reviewer reused its completed #65 skill context due to the slot limit. No reviewer switched arms or read another review. This one within-arm carryover limits fresh-context equivalence.</li>
<li>Requested approximately six minutes per review, without a hard compute/token cap. Toolchain warmup, test execution and access to cached dependencies differed. No efficiency or cost claim is made.</li>
<li>Historical comments, later commits and amended PR descriptions were withheld. Skill steps that read existing comments or judge the PR description were disabled. This evaluates the code-review guidance, not the complete operational workflow as written.</li>
<li>The coordinator froze the historical rubric before reading outputs, then checked base/head traces, reproductions and historical dispositions. Adjudication was not blinded and has not had independent human review.</li>
<li>Focused Go, Node and shell checks are recorded per run. Some HTTP tests hit sandbox listener restrictions; one early run forced an older local Go version. No cloud deployment or live MDM lifecycle was tested. Full repository gates and Docker performance runs were not completed.</li>
</ul>
<h2>Recommendations</h2>
<ol>
<li><strong>Keep the skills as an additional review pass.</strong> In this sample they added five supported findings, while generic review contributed two that skills missed. A generic pass followed by a skill pass is a candidate workflow, not something this parallel-arm experiment directly tested.</li>
<li><strong>Fix Fleet Manager's comment-reading order.</strong> Its procedure reads existing threads before its own review; shipper defers them. Move reconciliation after independent investigation to avoid copying prior conclusions. The experiment had to override this step.</li>
<li><strong>Require actual behavior evidence for platform claims.</strong> A shell stub established flags, not Terraform approval behavior. A local-only native Terraform 1.5.7 test accepted yes with <code>-input=false</code>, contradicting generic's unconditional P1. Current official docs differ; other CLI versions remain untested.</li>
<li><strong>Validate on future PRs with the skills frozen.</strong> Use uncited PRs created after the freeze, fresh contexts for every run, repeated runs, measured token/time budgets and blinded human adjudication. Until then, describe the result as promising retrospective evidence, particularly for shipper lifecycle and state handling.</li>
</ol>
<h2>Finding-by-finding evidence</h2>
<p>Raw reviewer findings below are preserved in JSON. The adjudication is a separate layer; a review comment is never automatically treated as truth.</p>''')
    for case in manifest:
        parts.append('<h3>' + link(f"{root_url}/pull/{case['pr']}", f"#{case['pr']}") + ' — ' + esc(case['title']) + '</h3>')
        parts.append('<p class="small">Reviewed <code>' + case['head'] + '</code>; baseline <code>' + case['base'] + '</code>.</p>')
        for arm in ('generic', 'skill'):
            review = read(f"results/{case['case']}-{arm}.json")
            if not review['findings']:
                parts.append('<p>' + esc(arm.title()) + ': no actionable findings.</p>')
            for index, finding in enumerate(review['findings']):
                j = lookup[(case['case'], arm, index)]
                code_url = f"{root_url}/blob/{case['head']}/{finding['file']}#L{finding['line']}"
                parts.append('<details><summary>' + esc(arm.title() + ' · ' + finding['priority'] + ' · ' + j['verdict'] + ' · ' + finding['title']) + '</summary><p>' + link(code_url, f"{finding['file']}:{finding['line']}") + '</p><p>' + esc(finding['explanation']) + '</p><p><strong>Reviewer evidence:</strong> ' + esc(finding['evidence']) + '</p><p><strong>Adjudication:</strong> ' + esc(j['rationale']) + '</p></details>')
            parts.append('<details><summary>' + esc(arm.title()) + ' checks and limitations</summary><ul>' + ''.join('<li>' + esc(x) + '</li>' for x in review['checks'] + review['limitations']) + '</ul></details>')
    parts.append('''<h2>Audit files</h2><p>
<a href="protocol.json">Protocol</a> · <a href="manifest.json">Pinned revisions</a> ·
<a href="historical-rubric.json">Frozen historical rubric</a> · <a href="contamination.json">Training references</a> ·
<a href="judgments.json">Adjudications</a> · <a href="scores.json">Computed scores</a> ·
<a href="reproduce.txt">Reproduction instructions</a> · <a href="reviewer-instructions.txt">Exact shared reviewer prompt</a></p>
<p class="small">Platform sources used in adjudication:
<a href="https://developer.apple.com/documentation/XPC">Apple launch-agent process model</a>;
<a href="https://support.apple.com/en-ie/guide/directory-utility/diru978f56de/mac">Apple directory search policies</a>;
<a href="https://docs.cloud.google.com/run/docs/deploying">Cloud Run image digest behavior</a>;
<a href="https://docs.aws.amazon.com/AmazonECS/latest/APIReference/API_UpdateExpressGatewayService.html">ECS service updates</a>;
<a href="https://developer.hashicorp.com/terraform/cli/commands/apply">Terraform apply reference</a>.
Cloud outcomes are inferred from source and vendor behavior; the Terraform counterexample was executed locally.
</p></body></html>''')
    (ROOT / 'report.html').write_text('\n'.join(parts) + '\n')

if __name__ == '__main__':
    main()
