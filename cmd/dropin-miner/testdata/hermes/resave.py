#!/usr/bin/env python3
"""Regenerate the fixtures: what Hermes turns our hook into when it saves.

Hermes has more than one writer for config.yaml, and they do different things
to the block `agents install` writes. Both are copied here from utils.py at
NousResearch/hermes-agent d150fc202463, each with the arguments of its one
dump call. If Hermes changes either, change them here and regenerate; the
fixtures are this script's real output and are never typed or edited by hand.

<name>.resaved.yaml - atomic_yaml_write, the whole-file writer: the file is
loaded as data and dumped again through PyYAML (hermes_cli/config.py
save_config, the setup wizard, and `hermes config set` through
set_config_value -> _write_user_config). Our marker comments are dropped and
our entry is rewritten in PyYAML's style: a plain or single-quoted scalar,
folded at 80 columns.

<name>.roundtrip.yaml - atomic_roundtrip_yaml_update, the single-key writer: a
ruamel round trip (a model switch through persist_model_selection, an
in-session setting through cli.save_config_value, a personality change; the
TUI gateway's atomic_roundtrip_yaml_save shares its loader and its dump).
Comments and quotes are kept, so our markers survive and our matcher keeps
its quotes, and the command is folded onto a second line INSIDE them. The
change made here is hermes_cli/personality.py's, `display.personality`, which
also puts a key of Hermes' own after our block.

<name>.roundtrip-sibling.yaml and <name>.roundtrip-item.yaml - the same writer
adding to the hooks: mapping OUR block opened (#125): a second event under
hooks:, and an entry appended to our own pre_tool_call list. ruamel keeps our
end marker attached to the line above it, our matcher, so both land AFTER the
end marker and are still inside our mapping: a line at depth two, a line at
depth four. Cut marker to marker and what is left no longer parses.

Hermes writes through a text-mode handle (utils.py _atomic_write:
os.fdopen(fd, "w")), so on Windows every one of these ends its lines in CRLF.
.gitattributes keeps *.yaml LF in this repository, so the fixtures are LF and
the tests that need Windows' bytes replace the line endings themselves.

Each <name>.input.yaml is what `agents install` writes for one fixed entry.
hermes_resaved_test.go refuses to run against an input that the renderer no
longer produces, so a stale fixture fails loudly instead of testing the past.

    python3 resave.py    # needs Hermes' own pins: pyyaml==6.0.3 ruamel.yaml==0.18.17
"""
import io
import pathlib

import yaml
from ruamel.yaml import YAML
from ruamel.yaml.comments import CommentedMap


class IndentDumper(yaml.SafeDumper):
    def increase_indent(self, flow=False, indentless=False):  # noqa: ARG002
        return super().increase_indent(flow, False)


def roundtrip(text, change):
    """utils.py _roundtrip_load's loader, one change to the loaded config, _roundtrip_dump's dump."""
    yaml_rt = YAML(typ="rt")
    yaml_rt.preserve_quotes = True
    yaml_rt.allow_unicode = True
    yaml_rt.default_flow_style = False
    yaml_rt.indent(mapping=2, sequence=4, offset=2)
    data = yaml_rt.load(text)
    config = data if isinstance(data, CommentedMap) else CommentedMap(data or {})
    change(config)
    out = io.StringIO()
    yaml_rt.dump(config, out)
    return out.getvalue()


def roundtrip_update(text, section, key, value):
    """atomic_roundtrip_yaml_update for a two-part key: `section.key = value`."""

    def change(config):
        if not isinstance(config.get(section), CommentedMap):
            config[section] = CommentedMap()
        config[section][key] = value

    return roundtrip(text, change)


def roundtrip_append(text, section, key, item):
    """The same round trip with an entry appended to the list at `section.key`."""
    return roundtrip(text, lambda config: config[section][key].append(item))


def ours(data):
    return data["hooks"]["pre_tool_call"][0]


here = pathlib.Path(__file__).parent
for src in sorted(here.glob("*.input.yaml")):
    text = src.read_text(encoding="utf-8")
    data = yaml.safe_load(text)
    outputs = {
        ".resaved.yaml": yaml.dump(data, Dumper=IndentDumper, default_flow_style=False, sort_keys=False, allow_unicode=True),
        ".roundtrip.yaml": roundtrip_update(text, "display", "personality", "kawaii"),
        ".roundtrip-sibling.yaml": roundtrip_update(text, "hooks", "post_tool_call", [{"command": "echo hi", "matcher": "terminal"}]),
        ".roundtrip-item.yaml": roundtrip_append(text, "hooks", "pre_tool_call", {"command": "/usr/bin/other-hook", "matcher": "terminal"}),
    }
    for suffix, out in outputs.items():
        # What every test built on these takes for granted, asked of the parser
        # Hermes loads the file with: a writer changed our entry's bytes and
        # not its meaning.
        if ours(yaml.safe_load(out)) != ours(data):
            raise SystemExit(f"{src.name}{suffix}: our entry no longer means what install wrote")
        dst = src.with_name(src.name.replace(".input.yaml", suffix))
        dst.write_text(out, encoding="utf-8", newline="\n")
        print(f"{src.name} -> {dst.name}")
