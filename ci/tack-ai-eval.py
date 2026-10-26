#!/usr/bin/env python3
"""Opt-in real API evaluation: read llm.Config JSON from stdin, never persist it.

Build ci/tack-ai as a static Linux probe first. Tools run only in a read-only,
network-disabled container, without the model key or any production task data.
"""
import argparse
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import uuid


def command(*args, **kwargs):
    return subprocess.run(args, check=True, capture_output=True, text=True, timeout=30, **kwargs).stdout


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--image', default='artex:tack-review')
    p.add_argument('--probe', required=True)
    p.add_argument('--output', required=True)
    p.add_argument('--repeats', type=int, default=2)
    p.add_argument('--case', choices=('retry', 'cancel', 'platform'))
    a = p.parse_args()
    if not 1 <= a.repeats <= 3:
        p.error('repeats must be 1..3')
    config = json.load(sys.stdin)
    report_dir = Path(a.output); report_dir.mkdir(parents=True, exist_ok=True)
    project = Path(__file__).resolve().parents[1]
    with tempfile.TemporaryDirectory(prefix='artex-tack-ai-skills-') as temp:
        seeded = Path(temp) / 'skills'
        command(a.probe, '--seed', str(seeded))
        cases = {
            'retry': '使用 Tack 在 fixture-a 上执行一次脚本 printf x >> /work/once.marker; printf "ready\\n"。需要验证丢失提交回执时的重试方式：对这次命令重试提交，确认最终仍只执行一次，取得完整输出。保存请求文件 /work/request.json 和结论 /work/answer.json，结论包含 task_id、state、stdout。',
            'cancel': '使用 Tack 在 fixture-a 上运行 sleep 120，保存请求文件 /work/request.json。先做一次 250ms 的有限等待，核对等待超时后远端是否仍在执行，再明确取消并查询终态。保存 /work/answer.json，包含 task_id、wait_exit_code、state_after_wait、cancel_requested_after_wait、final_state。',
            'platform': '准备一台 Linux x86_64、内核 2.6.32 的节点包，离线选包并 staging 到 /work/selected。另一个 Windows 7 amd64 设备是否有可用包？不要部署到任何设备。然后用 Tack 在 fixture-a 运行 exit 7，检查查询命令成功是否代表远端命令成功。保存 /work/answer.json，包含 selected_tool、platform、windows7_supported、task_id、state、exit_code。',
        }
        summaries = []
        for case, task in cases.items():
            if a.case and case != a.case:
                continue
            for round_number in range(1, a.repeats + 1):
                name = 'artex-tack-ai-' + uuid.uuid4().hex[:12]
                started = time.monotonic()
                prefix = report_dir / f'{case}-{round_number}'
                try:
                    command('docker', 'run', '-d', '--name', name, '--network', 'none', '--read-only',
                        '--user', '10001:10001', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
                        '--pids-limit', '128', '--memory', '768m', '--cpus', '2',
                        '--tmpfs', '/tmp:rw,mode=1777', '--tmpfs', '/work:rw,mode=1777', '-w', '/work', '-e', 'HOME=/work/home',
                        '-e', 'PATH=/work/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin',
                        '-v', str(Path(a.probe).resolve()) + ':/probe:ro',
                        '-v', str(seeded.resolve()) + ':/skills:ro',
                        '-v', str(project / 'ci/tack-ai-path.sh') + ':/etc/profile.d/99-tack-ai-path.sh:ro',
                        '-v', str(project / 'ci') + ':/test:ro', '--entrypoint', 'python3', a.image, '/test/tack_ai_fixture.py')
                    context = None
                    for attempt in range(100):
                        try:
                            context = json.loads(command('docker', 'exec', name, 'cat', '/work/context.json'))
                            break
                        except subprocess.CalledProcessError:
                            time.sleep(.1)
                    if context is None:
                        raise RuntimeError('fixture_not_ready')
                    prompt = prefix.with_suffix('.prompt.txt')
                    prompt.write_text(task + '\n这是隔离的合成环境，允许使用该节点和本机回环 HTTP，控制端与节点已运行。连接信息：' + json.dumps(context, ensure_ascii=False))
                    with prefix.with_suffix('.progress.txt').open('w') as progress:
                        result = subprocess.run([a.probe, '--container', name, '--skills', str(seeded), '--prompt', str(prompt)],
                            input=json.dumps(config), text=True, capture_output=False, stdout=subprocess.PIPE, stderr=progress, timeout=780)
                    if result.returncode:
                        raise RuntimeError('probe_failed')
                    report = json.loads(result.stdout)
                    prefix.with_suffix('.model.json').write_text(json.dumps(report, ensure_ascii=False, indent=2))
                    answer = json.loads(command('docker', 'exec', name, 'cat', '/work/answer.json'))
                    # Fetch authoritative records independently of the model's answer.
                    task_id = answer['task_id']
                    current = json.loads(command('docker', 'exec', name, 'tack', 'get', '--server', context['server'],
                        '--credential', context['credential_file'], '--allow-insecure-loopback', '--id', task_id, '--format', 'json'))
                    page = json.loads(command('docker', 'exec', name, 'tack', 'tasks', '--server', context['server'],
                        '--credential', context['credential_file'], '--allow-insecure-loopback', '--node', 'fixture-a'))
                    calls = [json.loads(line) for line in command('docker', 'exec', name, 'cat', '/work/cli-calls.jsonl').splitlines()]
                    checks = {'skill_used': report.get('calls', {}).get('Skill', 0) >= 1,
                              'model_completed': report.get('terminal') == 'completed',
                              'no_provider_error': not report.get('provider_error'),
                              'one_remote_execution': len(page['tasks']) == 1,
                              'output_complete': not current.get('result_pending', False)}
                    if case == 'retry':
                        submissions = [c for c in calls if c['args'] and c['args'][0] == 'submit']
                        checks['retry_exercised'] = len(submissions) >= 2 and all(c['exit_code'] == 0 for c in submissions)
                        checks['marker_once'] = command('docker', 'exec', name, 'cat', '/work/once.marker') == 'x'
                        checks['correct_result'] = answer['state'] == current['state'] == 'succeeded' and answer['stdout'] == current['result']['stdout'] == 'ready\n'
                        saved = json.loads(command('docker', 'exec', name, 'cat', '/work/request.json'))
                        checks['durable_request_identity'] = bool(saved.get('request_id')) and saved['request_id'] == current['request_id']
                    elif case == 'cancel':
                        checks['bounded_wait_exercised'] = any(c['args'][0] == 'wait' and c['exit_code'] == 4 for c in calls)
                        checks['cancel_exercised'] = any(c['args'][0] == 'cancel' and c['exit_code'] == 0 for c in calls)
                        checks['wait_not_cancel'] = answer['wait_exit_code'] == 4 and answer['state_after_wait'] in ('queued', 'dispatched', 'running') and answer['cancel_requested_after_wait'] is False
                        checks['canceled'] = answer['final_state'] == current['state'] == 'canceled'
                    else:
                        checks['correct_legacy'] = answer['selected_tool'] == 'tack-node-legacy' and answer['platform'] == 'linux/amd64'
                        checks['reject_unsupported'] = answer['windows7_supported'] is False
                        checks['correct_failure'] = answer['state'] == current['state'] == 'failed' and answer['exit_code'] == current['result']['exit_code'] == 7
                        receipt = json.loads(command('docker', 'exec', name, 'cat', '/work/selected/receipt.json'))
                        checks['staged_legacy'] = receipt['tool'] == 'tack-node-legacy'
                    summary = {'case': case, 'round': round_number, 'checks': checks, 'passed': all(checks.values()),
                               'elapsed_seconds': round(time.monotonic()-started, 1), 'turns': report.get('turns'), 'model': report.get('model')}
                    prefix.with_suffix('.receipt.json').write_text(json.dumps({'answer': answer, 'state': current['state'], 'checks': checks}, ensure_ascii=False, indent=2))
                except Exception as e:
                    summary = {'case': case, 'round': round_number, 'passed': False, 'error_class': type(e).__name__}
                    if isinstance(e, subprocess.CalledProcessError):
                        summary['failed_command'] = e.cmd[:5]
                finally:
                    subprocess.run(['docker', 'rm', '-f', name], capture_output=True, timeout=30)
                summaries.append(summary)
                (report_dir / 'summary.json').write_text(json.dumps(summaries, indent=2))
                print(json.dumps(summary), flush=True)
        return 0 if summaries and all(s['passed'] for s in summaries) else 1


if __name__ == '__main__':
    sys.exit(main())
