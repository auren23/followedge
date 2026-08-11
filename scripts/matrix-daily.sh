#!/usr/bin/env bash
# matrix-daily — daily v0.2.1 discovery snapshot (measurement freeze 4e761e9).
#
# Runs the three observation windows (24h/72h/168h) of the cross-actor
# matrix and writes a compact daily report to data/reports/matrix-daily.txt;
# full raw output is retained under data/reports/raw/. The report carries
# the reopen-trigger check: KEEP COLLECTING until ONE COMPLETE window has
# A/B/C each >= 10 (10-15 range; lower bound default) AND flagship pattern
# evaluability coverage on the discovery side >= 50%. The four numbers are
# never assembled across windows — each window is an independent cohort
# definition (ConsEV can flip sign across windows), so a "best of each
# window" trigger would fabricate a REACHED no single window earned.
#
# Fail-closed: if the matrix command fails (SQLite lock/corruption, binary
# crash, config error, disk full), the report says ERROR and the script
# exits non-zero so the systemd unit shows failed — a broken collector must
# never be disguised as "no data yet". A rank failure only degrades to WARN
# (rank does not participate in the trigger).
#
# REACHED is a signal for a HUMAN to review — the script never starts
# Discovery or changes any configuration on its own.
#
# SYNC: the compact report (dated copy + latest) is committed and pushed to
# the repo's origin so the longitudinal series is readable from GitHub in
# later sessions (the SQLite DB and raw per-window snapshots stay local).
# A sync failure only degrades to SYNC WARN — it never touches the trigger.
#
# Intended to run from a systemd user timer (Persistent=true) or cron.
# No measurement/cohort/pattern-gate logic lives here — this is ops only,
# the Go discovery code is frozen.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$REPO/bin/followedge"
REPORT="$REPO/data/reports/matrix-daily.txt"
RAW="$REPO/data/reports/raw"
HISTORY="$REPO/data/reports/history"
mkdir -p "$RAW" "$HISTORY"

# The /tmp tmpfs on this box hits its quota during go builds; point the
# go temp/build space at local disk.
export TMPDIR="${TMPDIR:-$HOME/.tmp-gotest}"
mkdir -p "$TMPDIR"

FREEZE_SHA="4e761e9"
DATE="$(date +%Y-%m-%d)"
THRESHOLD_A=10  # reopen range is A >= 10-15; default to the lower bound
THRESHOLD_B=10
THRESHOLD_C=10
COV_GATE=50     # flagship pattern side-A evaluability coverage, percent

if [ ! -x "$BIN" ]; then
    (cd "$REPO" && go build -o "$BIN" ./cmd/followedge)
fi

DB_MTIME="$(stat -c '%y' "$REPO/data/followedge.db" 2>/dev/null || echo 'n/a')"

# sync_report: commit + push the compact report (dated copy + latest) so it
# is readable from GitHub in later sessions. Best-effort — a failure is a
# SYNC WARN line in the report, never an ERROR (the trigger is about data,
# not about sync).
#
# Index isolation: a plain `git commit` commits EVERYTHING already staged.
# If the index holds files outside data/reports/ (e.g. a coding agent staged
# internal/foo.go but has not committed yet), the report commit would drag
# them along. Fail-safe: detect foreign staged files BEFORE staging the
# report and skip the auto-commit with a WARN — never touch the agent's
# index.
sync_report() {
    cp "$REPORT" "$HISTORY/matrix-$DATE.txt"
    if git -C "$REPO" diff --cached --name-only | grep -v '^data/reports/' | grep -q .; then
        echo "SYNC WARN — index has staged files outside data/reports/; skipped auto-commit (report is local only)" \
            | tee -a "$REPORT"
        return 0
    fi
    if git -C "$REPO" status --porcelain -- data/reports | grep -q .; then
        git -C "$REPO" add data/reports/matrix-daily.txt data/reports/history/
        if ! git -C "$REPO" commit -q -m "chore(reports): daily matrix snapshot $DATE"; then
            echo "SYNC WARN — git commit failed; report is local only" | tee -a "$REPORT"
            return 0
        fi
        if ! git -C "$REPO" push -q; then
            echo "SYNC WARN — git push failed; report committed locally only" | tee -a "$REPORT"
        fi
    fi
}

{
    echo "# Matrix Daily — $DATE (discovery freeze $FREEZE_SHA)"
    echo "# gates: min-repl 20/5 · quality 30 · horizon 5m"
    echo "# db last write: $DB_MTIME  (collector freshness)"
    echo
} > "$REPORT"

qualifying=""  # first window that clears ALL gates on its own
q_a=0; q_b=0; q_c=0; q_cov=0

for win in 24h 72h 168h; do
    # Fail closed: matrix is the trigger source — any failure must abort
    # with an ERROR report, not be read as "no data".
    matrix_err=""
    matrix_out="$("$BIN" actors matrix --since "$win" --horizon 5m 2>&1)" || matrix_err=$?
    if [ -n "$matrix_err" ]; then
        echo "$matrix_out" > "$RAW/matrix-daily-$DATE-$win.txt"
        {
            echo "--- $win ---"
            echo "MATRIX ERROR (exit $matrix_err):"
            echo "$matrix_out"
        } >> "$REPORT"
        echo "STATUS: ERROR — matrix command failed for $win (exit $matrix_err); trigger not evaluated" >> "$REPORT"
        sync_report
        exit 1
    fi

    rank_err=""
    rank_out="$("$BIN" actors rank --since "$win" --horizon 5m --limit 20 2>&1)" || rank_err=$?
    echo "$matrix_out" > "$RAW/matrix-daily-$DATE-$win.txt"
    echo "$rank_out"   > "$RAW/matrix-daily-$DATE-$win-rank.txt"

    # OUTCOME 2x2: "  quality high  A      6      B      4"
    a=$(echo "$matrix_out" | awk '/quality high/ {print $4}'); a=${a:-0}
    b=$(echo "$matrix_out" | awk '/quality high/ {print $6}'); b=${b:-0}
    c=$(echo "$matrix_out" | awk '/quality low/  {print $4}'); c=${c:-0}
    d=$(echo "$matrix_out" | awk '/quality low/  {print $6}'); d=${d:-0}
    # EVIDENCE COVERAGE: "  A    15   0    150   9   6    150" (research = $7)
    ra=$(echo "$matrix_out" | awk '/^  A /{print $7}'); ra=${ra:-0}
    rb=$(echo "$matrix_out" | awk '/^  B /{print $7}'); rb=${rb:-0}
    rc=$(echo "$matrix_out" | awk '/^  C /{print $7}'); rc=${rc:-0}
    rd=$(echo "$matrix_out" | awk '/^  D /{print $7}'); rd=${rd:-0}
    # flagship pattern side-A coverage in the PROFIT contrast (absent when
    # cell A is empty → "" → 0)
    cov=$(echo "$matrix_out" | awk '/CONTRAST: PROFIT/,0' \
        | grep -m1 'early independent entry' \
        | grep -o 'cov [0-9]*%' | head -1 | tr -dc '0-9' || true)
    cov=${cov:-0}

    # A window qualifies only when IT ALONE clears every gate — never mix
    # the best A from one window with the best B from another.
    if [ -z "$qualifying" ] \
       && [ "$a" -ge "$THRESHOLD_A" ] && [ "$b" -ge "$THRESHOLD_B" ] \
       && [ "$c" -ge "$THRESHOLD_C" ] && [ "$cov" -ge "$COV_GATE" ]; then
        qualifying="$win"
        q_a=$a; q_b=$b; q_c=$c; q_cov=$cov
    fi

    rank_note="ok"
    [ -n "$rank_err" ] && rank_note="WARN — rank failed (exit $rank_err); replication detail missing"

    {
        echo "--- $win ---"
        echo "cells:        A=$a B=$b C=$c D=$d"
        echo "research eps: A=$ra B=$rb C=$rc D=$rd"
        echo "flagship cov (side A): ${cov}%"
        echo "rank: $rank_note"
        echo "patterns/gates:"
        echo "$matrix_out" | sed -n '/PATTERNS — prevalence per side/,/^HYPOTHESES/p' \
            | grep -v '^HYPOTHESES' | head -16 || true
        echo "full raw: data/reports/raw/matrix-daily-$DATE-$win.txt (+ -rank.txt)"
        echo
    } >> "$REPORT"
done

if [ -n "$qualifying" ]; then
    status="REACHED — $qualifying qualifies (A=$q_a B=$q_b C=$q_c, flagship cov=${q_cov}%); HUMAN REVIEW required — script never auto-starts Discovery"
else
    status="KEEP COLLECTING — need at least ONE full window with A/B/C >= $THRESHOLD_A/$THRESHOLD_B/$THRESHOLD_C AND flagship cov >= ${COV_GATE}%; none qualified today"
fi

echo "STATUS: $status" >> "$REPORT"
sync_report
echo "$status"
