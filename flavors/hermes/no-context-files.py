# SPDX-License-Identifier: Apache-2.0
#
# Run once at image build (Dockerfile): patches Hermes so that no repository context file
# reaches the agent, while SOUL.md, which links to Koinon's instructions, still does. Then
# checks the result by building a system prompt.
#
# Hermes loads the session directory's .hermes.md/HERMES.md, AGENTS.md, CLAUDE.md or
# .cursorrules into the system prompt (agent/prompt_builder.py, build_context_files_prompt) and
# splices AGENTS.md and the like from subdirectories into tool results
# (agent/subdirectory_hints.py) unless the agent is built with skip_context_files. No setting
# turns that on for an ACP session without also dropping SOUL.md: the gateway passes
# skip_context_files=ignore_rules and load_soul_identity=not ignore_rules
# (gateway/session_execution.py, _build_fresh_agent). With load_soul_identity, SOUL.md stays
# in the identity slot when context files are skipped (agent/system_prompt.py,
# _identity_parts). So every agent is built with skip_context_files, and with
# load_soul_identity wherever it would have read SOUL.md before. See flavors/hermes/README.md.
#
# Usage: python -I -B no-context-files.py patch | check
import os
import sys
import tempfile
from pathlib import Path

TARGET = Path("/opt/hermes/agent/agent_init.py")
ANCHOR = """    if safe_worker_enabled():
        skip_context_files = skip_memory = skip_background_review = True
        load_soul_identity = False
        prefill_messages = []
        fallback_model = {}
    _install_safe_stdio()
"""
PATCHED = """    if safe_worker_enabled():
        skip_context_files = skip_memory = skip_background_review = True
        load_soul_identity = False
        prefill_messages = []
        fallback_model = {}
    # Koinon: no repository context file reaches the agent; SOUL.md still does where it did.
    load_soul_identity = load_soul_identity or not skip_context_files
    skip_context_files = True
    _install_safe_stdio()
"""


def patch():
    src = TARGET.read_text()
    if src.count(ANCHOR) != 1:
        sys.exit(f"{TARGET}: the code to patch is not there exactly once; Hermes changed")
    TARGET.write_text(src.replace(ANCHOR, PATCHED))


def check():
    home, repo = Path(tempfile.mkdtemp()), Path(tempfile.mkdtemp())
    (home / "SOUL.md").write_text("KOINON-SOUL-CHECK\n")
    for name in (".hermes.md", "AGENTS.md", "CLAUDE.md", ".cursorrules"):
        (repo / name).write_text(f"KOINON-REPO-CHECK {name}\n")
    os.environ["HERMES_HOME"] = str(home)
    os.chdir(repo)
    sys.path.insert(0, "/opt/hermes")
    from run_agent import AIAgent
    from agent.system_prompt import build_system_prompt_parts

    # The ACP session's construction (gateway/session_execution.py): a frozen policy without
    # ignore_rules.
    agent = AIAgent(model="check", provider="custom", base_url="http://127.0.0.1:9/v1", api_key="check",
                    quiet_mode=True, skip_memory=True, enabled_toolsets=[], cwd=str(repo), platform="acp",
                    skip_context_files=False, load_soul_identity=True)
    prompt = "\n".join(build_system_prompt_parts(agent).values())
    if "KOINON-SOUL-CHECK" not in prompt:
        sys.exit("SOUL.md is not in the system prompt")
    if "KOINON-REPO-CHECK" in prompt:
        sys.exit("a repository context file is in the system prompt")
    if agent._subdirectory_hints.enabled:
        sys.exit("subdirectory context hints are on")


{"patch": patch, "check": check}[sys.argv[1]]()
