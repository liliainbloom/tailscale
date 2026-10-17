#!/usr/bin/env python3
"""Compare upstream and fork DNS with the same tests, compiler and machine.

Only the two changed production files are overlaid with their upstream versions;
the working checkout is never modified. Runs alternate order to reduce drift.
Keep the output directory outside the checkout: it contains local measurements.
"""
import argparse
import hashlib
import json
import pathlib
import platform
import statistics
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", default="go")
    parser.add_argument("--base", default="v1.104.1")
    parser.add_argument("--output", required=True)
    parser.add_argument("--samples", type=int, default=6)
    parser.add_argument("--benchtime", default="500ms")
    parser.add_argument("--bench", default="^BenchmarkDNSRouteBaseline$",
                        help="Go benchmark regexp, optionally selecting a sub-benchmark")
    args = parser.parse_args()
    if args.samples < 1:
        parser.error("--samples must be positive")
    root = pathlib.Path(__file__).resolve().parent.parent
    out = pathlib.Path(args.output).resolve()
    if out.is_relative_to(root):
        parser.error("keep measurement output outside the Git checkout")
    out.mkdir(parents=True, exist_ok=True)

    def command(argv, **kwargs):
        return subprocess.run(argv, cwd=root, check=True, **kwargs)

    replacements = {}
    hashes = {}
    for name in ["forwarder.go", "tsdns.go"]:
        path = "net/dns/resolver/" + name
        original = command(["git", "show", args.base + ":" + path], capture_output=True).stdout
        copy = out / ("upstream-" + name)
        copy.write_bytes(original)
        replacements[str(root / path)] = str(copy)
        hashes[path] = hashlib.sha256((root / path).read_bytes()).hexdigest()
    overlay = out / "overlay.json"
    overlay.write_text(json.dumps({"Replace": replacements}, indent=2) + "\n")
    for label in ["upstream", "fork"]:
        print("Building " + label, flush=True)
        cmd = [args.go, "test", "-c", "-o", str(out / (label + ".test"))]
        if label == "upstream":
            cmd += ["-overlay", str(overlay)]
        cmd += ["./net/dns/resolver"]
        command(cmd)
        (out / (label + ".txt")).write_text("")

    samples = {"upstream": {}, "fork": {}}
    for repeat in range(args.samples):
        order = ["upstream", "fork"] if repeat % 2 == 0 else ["fork", "upstream"]
        for label in order:
            print(f"Sample {repeat + 1}/{args.samples}: {label}", flush=True)
            text = command([str(out / (label + ".test")), "-test.run=^$",
                            "-test.bench=" + args.bench, "-test.benchmem",
                            "-test.benchtime=" + args.benchtime], capture_output=True, text=True).stdout
            with (out / (label + ".txt")).open("a") as f:
                f.write(text)
            for line in text.splitlines():
                fields = line.split()
                if fields and fields[0].startswith("BenchmarkDNSRouteBaseline/"):
                    name = fields[0].split("/", 1)[1].rsplit("-", 1)[0]
                    samples[label].setdefault(name, []).append({
                        "ns_per_op": float(fields[2]), "bytes_per_op": int(fields[4]),
                        "allocs_per_op": int(fields[6]),
                    })

    summary = {}
    for label, cases in samples.items():
        summary[label] = {
            name: {metric: statistics.median(s[metric] for s in runs) for metric in runs[0]}
            for name, runs in cases.items()
        }
    result = {
        "base": args.base,
        "base_commit": command(["git", "rev-parse", args.base + "^{commit}"], capture_output=True, text=True).stdout.strip(),
        "go_version": command([args.go, "version"], capture_output=True, text=True).stdout.strip(),
        "platform": platform.platform(), "production_sha256": hashes,
        "samples_per_case": args.samples, "benchtime": args.benchtime, "bench": args.bench,
        "summary_medians": summary, "samples": samples,
        "limitations": "Local loopback DNS microbenchmarks; medians are descriptive, not a significance test or VPN throughput measurement.",
    }
    (out / "summary.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(summary, indent=2))


if __name__ == "__main__":
    main()
