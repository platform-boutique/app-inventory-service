#!/usr/bin/env python3
"""Copy the stock numbers of the catalog seed (products.json) into inventory-service's seed (data/stock.json).

Usage: python3 tools/sync_stock.py [PATH/TO/products.json] [-o data/stock.json] [--check]

Input:  a JSON array of products (or {"products": [...]}), each with "id" and "stock" (integer >= 0).
        Default path: ../product-catalog-service/data/seed/products.json (relative to this repo).
Output: a JSON array of {"product_id", "available"}, sorted by product id, so re-running it gives a
        byte-identical file.
--check writes nothing: exit 0 when the file matches the catalog, 1 (with the differences) when it
        has drifted, 2 when the catalog file is not there.
Standard library only.
"""

import argparse
import json
import os
import sys

MAX_PRODUCT_ID = 128
HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_SRC = os.path.join(HERE, "..", "..", "product-catalog-service", "data", "seed", "products.json")
DEFAULT_OUT = os.path.join(HERE, "..", "data", "stock.json")


def convert(products):
    out, problems, seen = [], [], set()
    for p in products:
        pid = str(p.get("id", "")).strip()
        stock = p.get("stock")
        if not pid or len(pid) > MAX_PRODUCT_ID:
            problems.append(f"product without a usable id: {p.get('id')!r}")
        elif pid in seen:
            problems.append(f"{pid}: duplicate product id")
        elif not isinstance(stock, int) or isinstance(stock, bool) or not 0 <= stock <= 1_000_000_000:
            problems.append(f"{pid}: stock {stock!r} is not an integer from 0 to 1000000000")
        else:
            seen.add(pid)
            out.append({"product_id": pid, "available": stock})
    out.sort(key=lambda x: x["product_id"])
    return out, problems


def render(rows):
    return json.dumps(rows, ensure_ascii=False, indent=1) + "\n"


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("products", nargs="?", default=DEFAULT_SRC, help="path to the catalog's products.json")
    ap.add_argument("-o", "--out", default=DEFAULT_OUT, help="output file (default: data/stock.json)")
    ap.add_argument("--check", action="store_true", help="do not write; fail when the file differs from the catalog")
    args = ap.parse_args()

    if not os.path.exists(args.products):
        print(f"catalog seed not found: {args.products}", file=sys.stderr)
        sys.exit(2)
    with open(args.products, encoding="utf-8") as f:
        doc = json.load(f)
    products = doc.get("products") if isinstance(doc, dict) else doc
    if not isinstance(products, list):
        sys.exit("input must be a JSON array of products or an object with a 'products' array")

    rows, problems = convert(products)
    for msg in problems:
        print("problem:", msg, file=sys.stderr)
    if problems:
        sys.exit(1)

    if args.check:
        try:
            with open(args.out, encoding="utf-8") as f:
                have = json.load(f)
        except (OSError, ValueError) as e:
            print(f"cannot read {args.out}: {e}", file=sys.stderr)
            sys.exit(1)
        want = {r["product_id"]: r["available"] for r in rows}
        got = {r["product_id"]: r["available"] for r in have}
        diffs = [f"{k}: file {got.get(k)} catalog {want.get(k)}" for k in sorted(set(want) | set(got)) if want.get(k) != got.get(k)]
        if diffs or render(rows) != open(args.out, encoding="utf-8").read():
            for d in diffs:
                print("drift:", d, file=sys.stderr)
            print(f"{args.out} is out of date: run python3 tools/sync_stock.py", file=sys.stderr)
            sys.exit(1)
        print(f"ok: {len(rows)} products, {args.out} matches the catalog", file=sys.stderr)
        return

    with open(args.out, "w", encoding="utf-8") as f:
        f.write(render(rows))
    print(f"wrote stock of {len(rows)} products to {args.out}", file=sys.stderr)


if __name__ == "__main__":
    main()
