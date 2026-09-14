# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0

"""Render the luxd-vs-LiteLLM comparison charts from tidy per-trial data.

Input is ``results.csv`` (the data behind the committed figures), the tidy
per-trial table the load driver writes with ``driver csv``:

    trial,shape,mode,subject,metric,value

where ``shape`` is the request group (passthrough or translated), ``mode`` is
nonstream or stream, ``subject`` is baseline, luxd, or litellm, and ``metric``
is one of p50_ms..p99_ms, reqs_per_sec, or peak_rss_mb. Every condition is
measured over several independent trials; this script aggregates each
condition and metric to a central value (the median across trials) and a
bootstrap 95% confidence interval, writes that aggregate to a CSV, and draws:

  * latency-percentiles.png -- p50..p99 as lines on a log y-axis (LiteLLM is
    ~100x, so a linear axis would flatten luxd against zero), one line per
    subject with a shaded 95% CI band, faceted by shape x mode.
  * throughput.png -- requests per second as bars on a log y-axis (subjects
    differ by ~200x) with 95% CI error bars, faceted the same way.

Output is deterministic: fixed figure size and dpi, a seeded bootstrap, and
stripped PNG metadata, so a re-render of the same data is byte-stable. Charts
are saved with ``bbox_inches='tight'``.

Usage:
    python render.py                       # reads ./results.csv, writes ./figures
    python render.py --csv X --figures Y   # explicit paths (run.sh uses these)
"""

from __future__ import annotations

import argparse
import pathlib

import matplotlib

matplotlib.use("Agg")  # no display; deterministic file output

import matplotlib.pyplot as plt  # noqa: E402
import numpy as np  # noqa: E402
import pandas as pd  # noqa: E402
import seaborn as sns  # noqa: E402

# Fixed presentation choices so every render is identical for the same data.
DPI = 150
BOOTSTRAP_RESAMPLES = 10000
BOOTSTRAP_SEED = 0

# Subjects: a colourblind-safe grey/blue/orange, baseline muted on purpose.
SUBJECT_ORDER = ["baseline", "luxd", "litellm"]
SUBJECT_LABEL = {"baseline": "baseline (direct to mock)", "luxd": "luxd", "litellm": "LiteLLM"}
PALETTE = {"baseline": "#595959", "luxd": "#0173B2", "litellm": "#DE8F05"}

SHAPE_ORDER = ["passthrough", "translated"]
SHAPE_LABEL = {
    "passthrough": "Passthrough (OpenAI in, OpenAI upstream)",
    "translated": "Translated (OpenAI in, Anthropic upstream)",
}
MODE_ORDER = ["nonstream", "stream"]
MODE_LABEL = {"nonstream": "Non-streaming", "stream": "Streaming (SSE)"}

PCTL_METRICS = ["p50_ms", "p75_ms", "p90_ms", "p95_ms", "p99_ms"]
PCTL_LABEL = {"p50_ms": "p50", "p75_ms": "p75", "p90_ms": "p90", "p95_ms": "p95", "p99_ms": "p99"}


def bootstrap_median_ci(values: np.ndarray) -> tuple[float, float, float]:
    """Return (median, ci_low, ci_high) with a seeded percentile bootstrap.

    A single trial has no spread, so its interval is the point itself.
    """
    values = np.asarray(values, dtype=float)
    center = float(np.median(values))
    if values.size < 2:
        return center, center, center
    rng = np.random.default_rng(BOOTSTRAP_SEED)
    idx = rng.integers(0, values.size, size=(BOOTSTRAP_RESAMPLES, values.size))
    medians = np.median(values[idx], axis=1)
    lo, hi = np.percentile(medians, [2.5, 97.5])
    return center, float(lo), float(hi)


def aggregate(df: pd.DataFrame) -> pd.DataFrame:
    """Collapse per-trial rows to one row per condition and metric."""
    rows = []
    keys = ["shape", "mode", "subject", "metric"]
    for (shape, mode, subject, metric), g in df.groupby(keys, sort=False):
        vals = g["value"].to_numpy(dtype=float)
        center, lo, hi = bootstrap_median_ci(vals)
        rows.append(
            {
                "shape": shape,
                "mode": mode,
                "subject": subject,
                "metric": metric,
                "n": int(vals.size),
                "center": center,
                "ci_lo": lo,
                "ci_hi": hi,
                "mean": float(vals.mean()),
                "std": float(vals.std(ddof=1)) if vals.size > 1 else 0.0,
            }
        )
    agg = pd.DataFrame(rows)
    agg["shape"] = pd.Categorical(agg["shape"], categories=SHAPE_ORDER, ordered=True)
    agg["mode"] = pd.Categorical(agg["mode"], categories=MODE_ORDER, ordered=True)
    agg["subject"] = pd.Categorical(agg["subject"], categories=SUBJECT_ORDER, ordered=True)
    return agg.sort_values(["shape", "mode", "subject", "metric"]).reset_index(drop=True)


def _facet_axes():
    fig, axes = plt.subplots(
        len(SHAPE_ORDER), len(MODE_ORDER), figsize=(11.0, 8.0), sharex=True
    )
    return fig, axes


def render_latency(agg: pd.DataFrame, out: pathlib.Path) -> None:
    """p50..p99 lines with a 95% CI band, log y, faceted by shape x mode."""
    lat = agg[agg["metric"].isin(PCTL_METRICS)].copy()
    positions = np.arange(len(PCTL_METRICS))
    fig, axes = _facet_axes()
    for r, shape in enumerate(SHAPE_ORDER):
        for c, mode in enumerate(MODE_ORDER):
            ax = axes[r][c]
            cell = lat[(lat["shape"] == shape) & (lat["mode"] == mode)]
            for subject in SUBJECT_ORDER:
                s = cell[cell["subject"] == subject].set_index("metric").reindex(PCTL_METRICS)
                if s["center"].isna().all():
                    continue
                color = PALETTE[subject]
                ax.plot(
                    positions, s["center"], marker="o", color=color,
                    label=SUBJECT_LABEL[subject], linewidth=1.8, markersize=5,
                )
                ax.fill_between(positions, s["ci_lo"], s["ci_hi"], color=color, alpha=0.18, linewidth=0)
            ax.set_yscale("log")
            ax.set_title(f"{SHAPE_LABEL[shape]}\n{MODE_LABEL[mode]}", fontsize=10)
            ax.set_xticks(positions)
            ax.set_xticklabels([PCTL_LABEL[m] for m in PCTL_METRICS])
            ax.grid(True, which="both", axis="y", alpha=0.3)
            if c == 0:
                ax.set_ylabel("added + total latency (ms, log scale)")
            if r == len(SHAPE_ORDER) - 1:
                ax.set_xlabel("latency percentile")
    handles, labels = axes[0][0].get_legend_handles_labels()
    fig.legend(handles, labels, loc="upper center", ncol=3, frameon=False, bbox_to_anchor=(0.5, 1.02))
    fig.suptitle(
        "Per-request latency by percentile (median of trials, 95% CI band)",
        y=1.06, fontsize=12,
    )
    fig.tight_layout()
    fig.savefig(out, dpi=DPI, bbox_inches="tight", metadata={"Software": None})
    plt.close(fig)


def render_throughput(agg: pd.DataFrame, out: pathlib.Path) -> None:
    """Requests per second as bars with 95% CI error bars, log y."""
    thr = agg[agg["metric"] == "reqs_per_sec"].copy()
    x = np.arange(len(SUBJECT_ORDER))
    fig, axes = _facet_axes()
    for r, shape in enumerate(SHAPE_ORDER):
        for c, mode in enumerate(MODE_ORDER):
            ax = axes[r][c]
            cell = thr[(thr["shape"] == shape) & (thr["mode"] == mode)].set_index("subject").reindex(SUBJECT_ORDER)
            centers = cell["center"].to_numpy(dtype=float)
            lo = np.clip(centers - cell["ci_lo"].to_numpy(dtype=float), 0, None)
            hi = np.clip(cell["ci_hi"].to_numpy(dtype=float) - centers, 0, None)
            colors = [PALETTE[s] for s in SUBJECT_ORDER]
            ax.bar(x, centers, color=colors, yerr=[lo, hi], capsize=4, error_kw={"linewidth": 1.0})
            ax.set_yscale("log")
            for xi, v in zip(x, centers):
                if np.isfinite(v):
                    ax.annotate(
                        f"{v:,.0f}", (xi, v), textcoords="offset points", xytext=(0, 4),
                        ha="center", va="bottom", fontsize=8,
                    )
            ax.set_title(f"{SHAPE_LABEL[shape]}\n{MODE_LABEL[mode]}", fontsize=10)
            ax.set_xticks(x)
            ax.set_xticklabels([SUBJECT_LABEL[s] for s in SUBJECT_ORDER], fontsize=9)
            ax.grid(True, which="both", axis="y", alpha=0.3)
            if c == 0:
                ax.set_ylabel("throughput (req/s, log scale)")
    fig.suptitle(
        "Single-process throughput (median of trials, 95% CI error bars)",
        y=1.02, fontsize=12,
    )
    fig.tight_layout()
    fig.savefig(out, dpi=DPI, bbox_inches="tight", metadata={"Software": None})
    plt.close(fig)


def main() -> None:
    here = pathlib.Path(__file__).resolve().parent
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--csv", type=pathlib.Path, default=here / "results.csv",
                    help="tidy per-trial CSV (default: ./results.csv)")
    ap.add_argument("--figures", type=pathlib.Path, default=here / "figures",
                    help="output directory for the PNGs (default: ./figures)")
    ap.add_argument("--aggregate", type=pathlib.Path, default=here / "results-aggregate.csv",
                    help="aggregate CSV to write (default: ./results-aggregate.csv)")
    args = ap.parse_args()

    df = pd.read_csv(args.csv)
    df["value"] = pd.to_numeric(df["value"], errors="coerce")
    df = df.dropna(subset=["value"])
    agg = aggregate(df)

    args.figures.mkdir(parents=True, exist_ok=True)
    args.aggregate.write_text(agg.to_csv(index=False))

    sns.set_theme(style="whitegrid", context="notebook")
    render_latency(agg, args.figures / "latency-percentiles.png")
    render_throughput(agg, args.figures / "throughput.png")

    n = int(df.groupby(["shape", "mode", "subject"])["trial"].nunique().max())
    print(f"rendered from {args.csv} (max N={n} trials/condition)")
    print(f"  {args.figures / 'latency-percentiles.png'}")
    print(f"  {args.figures / 'throughput.png'}")
    print(f"  {args.aggregate}")


if __name__ == "__main__":
    main()
