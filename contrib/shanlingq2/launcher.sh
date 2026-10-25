#!/bin/sh
# Installed as /mnt/mmc/.soulseek/run. Q2 Pod starts it from Streaming's Soulseek row (a card app,
# q2-pod's patch/addon.c). It runs from /tmp, never the card: the app and this launcher are
# copied there first and the card's copy of this script is left at once (exec), so nothing keeps
# the card busy and USB storage can always take it. The loop restarts the app after a failure,
# waiting 5 s and doubling to a minute; an exit of 0 (stopped from the Q2 or the phone, or idle) ends
# it. Q2 Pod stops it with a TERM to the loop's pid in /tmp/q2-soulseek, its downloads saved; an app
# still there 15 s after its TERM (frozen) is killed, so Stop always stops it.
pid=/tmp/q2-soulseek
if [ "$1" != --loop ]; then
    [ -f $pid ] && kill -0 "$(cat $pid)" 2>/dev/null && exit 0
    dir=$(cd "$(dirname "$0")" && pwd)
    cp -f "$dir/q2-soulseek" /tmp/q2-soulseek.bin && chmod 755 /tmp/q2-soulseek.bin || exit 1
    cp -f "$0" /tmp/q2-soulseek.run || exit 1
    cd /tmp
    exec /bin/sh /tmp/q2-soulseek.run --loop "$dir"
fi
dir=$2
(
    child=
    stop() {
        if [ -n "$child" ] && kill -TERM $child 2>/dev/null; then
            (sleep 15; kill -KILL $child 2>/dev/null) &
            k=$!
            wait $child
            kill $k 2>/dev/null
        fi
        rm -f $pid /tmp/q2-soulseek.bin /tmp/q2-soulseek.run
        exit 0
    }
    trap stop TERM INT
    delay=5
    while :; do
        /tmp/q2-soulseek.bin -dir "$dir" &
        child=$!
        wait $child
        code=$?
        [ $code = 0 ] && break
        sleep $delay
        [ $delay -lt 60 ] && delay=$((delay * 2))
    done
    rm -f $pid /tmp/q2-soulseek.bin /tmp/q2-soulseek.run
) </dev/null >/dev/null 2>&1 &
echo $! > $pid
