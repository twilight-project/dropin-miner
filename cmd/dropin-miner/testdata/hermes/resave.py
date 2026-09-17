#!/usr/bin/env python3
"""Regenerate the *.resaved.yaml fixtures: what Hermes turns our hook into.

Hermes saves config.yaml by loading it as data and dumping it again
(hermes_cli/config.py save_config -> utils.atomic_yaml_write), so the first
save after `agents install` drops our marker comments and rewrites our entry
in PyYAML's style: a plain or single-quoted scalar, folded at 80 columns.
The dumper below is Hermes' own, copied from utils.py at NousResearch/
hermes-agent d150fc202463, together with the arguments of its one dump call.
If Hermes changes either, change them here and regenerate; the fixtures are
this script's real output and are never typed or edited by hand.

Each <name>.input.yaml is what `agents install` writes for one fixed entry.
hermes_resaved_test.go refuses to run against an input that the renderer no
longer produces, so a stale fixture fails loudly instead of testing the past.

    python3 resave.py            # needs PyYAML (the parser Hermes uses)
"""
import pathlib

import yaml


class IndentDumper(yaml.Dumper):
    def increase_indent(self, flow=False, indentless=False):  # noqa: ARG002
        return super().increase_indent(flow, False)


here = pathlib.Path(__file__).parent
for src in sorted(here.glob("*.input.yaml")):
    data = yaml.safe_load(src.read_text(encoding="utf-8"))
    out = yaml.dump(data, Dumper=IndentDumper, default_flow_style=False, sort_keys=False, allow_unicode=True)
    dst = src.with_name(src.name.replace(".input.yaml", ".resaved.yaml"))
    dst.write_text(out, encoding="utf-8", newline="\n")
    print(f"{src.name} -> {dst.name}")
