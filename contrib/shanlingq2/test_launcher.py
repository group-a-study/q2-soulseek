#!/usr/bin/env python3
"""Run launcher.sh (the card's run) on the host with a fake app: after a failure it restarts, waiting
5 s and doubling; an exit of 0 ends the loop and tidies /tmp; a second run while it runs starts
nothing; a TERM to the loop's pid stops the app and tidies, and kills one that ignores it.
Usage: test_launcher.py"""
import os
import pathlib
import re
import signal
import subprocess
import tempfile
import time

source = pathlib.Path(__file__).with_name('launcher.sh').read_text()
app = re.search(r'^pid=/tmp/q2-([a-z]+)$', source, re.M)[1]


def wait(ok, what, secs=10):
    end = time.time() + secs
    while not ok():
        if time.time() > end:
            raise SystemExit(f'test_launcher.py: timed out waiting for {what}')
        time.sleep(0.02)


def rig(tmp, body):
    """The card's folder with the fake app and the launcher, /tmp's stand-in, and an environment
    whose sleep only records its argument, but for the stop's 15 s before a KILL: 1 s."""
    card, t, bin_ = tmp/'card'/f'.{app}', tmp/'t', tmp/'bin'
    for d in (card, t, bin_):
        d.mkdir(parents=True)
    exe = card/f'q2-{app}'
    exe.write_text('#!/bin/sh\n' + body)
    exe.chmod(0o755)
    (card/'run').write_text(source.replace(f'/tmp/q2-{app}', f'{t}/q2-{app}').replace('cd /tmp\n', f'cd {t}\n'))
    (bin_/'sleep').write_text('#!/bin/sh\necho "$1" >> "$Q2_TEST/sleeps"\n[ "$1" = 15 ] && exec /bin/sleep 1\nexit 0\n')
    (bin_/'sleep').chmod(0o755)
    return card, t, dict(os.environ, Q2_TEST=str(tmp), PATH=f'{bin_}:/usr/bin:/bin')


def lines(path):
    return path.read_text().splitlines() if path.exists() else []


with tempfile.TemporaryDirectory(prefix='q2-launcher-') as d:
    tmp = pathlib.Path(d)
    card, t, env = rig(tmp, 'echo "$@" >> "$Q2_TEST/args"\n[ "$(wc -l < "$Q2_TEST/args")" -ge 3 ] && exit 0\nexit 1\n')
    subprocess.run(['/bin/sh', str(card/'run')], env=env, check=True, timeout=10)
    wait(lambda: len(lines(tmp/'args')) == 3 and not (t/f'q2-{app}').exists(), 'the loop to end')
    assert lines(tmp/'args') == [f'-dir {card}'] * 3, lines(tmp/'args')
    assert lines(tmp/'sleeps') == ['5', '10'], lines(tmp/'sleeps')
    assert not list(t.iterdir()), list(t.iterdir())  # the pid, the copied app and launcher gone

with tempfile.TemporaryDirectory(prefix='q2-launcher-') as d:
    tmp = pathlib.Path(d)
    card, t, env = rig(tmp, 'echo start >> "$Q2_TEST/starts"\n'
                            'trap \'echo term >> "$Q2_TEST/terms"; exit 0\' TERM\n'
                            'while :; do /bin/sleep 0.05; done\n')
    subprocess.run(['/bin/sh', str(card/'run')], env=env, check=True, timeout=10)
    wait(lambda: lines(tmp/'starts') == ['start'], 'the app')
    subprocess.run(['/bin/sh', str(card/'run')], env=env, check=True, timeout=10)  # runs already
    time.sleep(0.3)
    assert lines(tmp/'starts') == ['start'], lines(tmp/'starts')
    os.kill(int((t/f'q2-{app}').read_text()), signal.SIGTERM)
    wait(lambda: not list(t.iterdir()), 'the TERM to tidy')
    assert lines(tmp/'terms') == ['term'] and lines(tmp/'sleeps') == ['15'], (lines(tmp/'terms'), lines(tmp/'sleeps'))

with tempfile.TemporaryDirectory(prefix='q2-launcher-') as d:  # frozen: TERM ignored
    tmp = pathlib.Path(d)
    card, t, env = rig(tmp, 'echo $$ >> "$Q2_TEST/starts"\ntrap "" TERM\nwhile :; do /bin/sleep 0.05; done\n')
    subprocess.run(['/bin/sh', str(card/'run')], env=env, check=True, timeout=10)
    wait(lambda: len(lines(tmp/'starts')) == 1, 'the app')
    os.kill(int((t/f'q2-{app}').read_text()), signal.SIGTERM)
    wait(lambda: not list(t.iterdir()), 'the KILL to tidy', 5)
    try:
        os.kill(int(lines(tmp/'starts')[0]), 0)
        raise SystemExit('test_launcher.py: the frozen app is still running')
    except ProcessLookupError:
        pass
print(f'Launcher (q2-{app}): restarts with backoff, exit 0, one at a time, TERM passed and a frozen app killed.')
