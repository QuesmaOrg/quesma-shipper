#!/usr/bin/env python3
"""Validate review evidence and recompute the paired, manually adjudicated scores."""
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parent

def read(name):
    return json.loads((ROOT / name).read_text())


def main():
    manifest = read("manifest.json")
    rubric = read("historical-rubric.json")["items"]
    judgments = read("judgments.json")
    keys = [(j["case"], j["arm"], j["index"]) for j in judgments]
    assert len(keys) == len(set(keys)), "Duplicate finding judgments"
    lookup = dict(zip(keys, judgments))
    historical = {r["id"]: r for r in rubric}
    summary = []
    for case in manifest:
        row = {"case": case["case"], "pr": case["pr"], "component": case["component"]}
        labels = {r["id"] for r in rubric if r["case"] == case["case"] and r["kind"] != "maintainability"}
        row["historical_denominator"] = len(labels)
        for arm in ("generic", "skill"):
            review = read(f"results/{case['case']}-{arm}.json")
            assert review["head"] == case["head"], "Wrong review SHA"
            assert review["case"] == case["case"] and review["arm"] == arm
            counts = {"supported": 0, "maintainability": 0, "false_positive": 0, "pre_existing": 0, "uncertain": 0}
            recovered = set()
            issues = set()
            for index, finding in enumerate(review["findings"]):
                j = lookup[(case["case"], arm, index)]
                assert finding["priority"] in ("P1", "P2", "P3")
                assert isinstance(finding["line"], int) and finding["line"] > 0
                assert j["issue"] not in issues, "Merge duplicate root causes before scoring"
                issues.add(j["issue"])
                counts[j["verdict"]] += 1
                for label in j["historical"]:
                    assert label in historical and historical[label]["case"] == case["case"]
                    assert j["verdict"] in ("supported", "maintainability")
                    if label in labels:
                        recovered.add(label)
            row[arm] = {**counts, "reported": len(review["findings"]), "historical_recovered": len(recovered), "historical_ids": sorted(recovered)}
        summary.append(row)
    assert len(lookup) == sum(r[a]["reported"] for r in summary for a in ("generic", "skill")), "Unused judgments"
    result = {"cases": summary}
    for component in ("fleet-manager", "shipper", "all"):
        subset = [r for r in summary if component == "all" or r["component"] == component]
        totals = {"historical_denominator": sum(r["historical_denominator"] for r in subset)}
        for arm in ("generic", "skill"):
            totals[arm] = {key: sum(r[arm][key] for r in subset) for key in ("reported", "supported", "maintainability", "false_positive", "pre_existing", "uncertain", "historical_recovered")}
        result[component] = totals
    (ROOT / "scores.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result, indent=2))

if __name__ == "__main__":
    main()
