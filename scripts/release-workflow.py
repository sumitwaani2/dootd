#!/usr/bin/env python3
"""Run an app's release workflow locally, without publishing.

    scripts/release-workflow.py <app dir> [--publish-to DIR]

Executes the `run:` steps of <app dir>/.github/workflows/release.yml in order,
from <app dir>, with the workflow's env block, the way GitHub Actions would
(GITHUB_PATH is honoured; `uses:` steps are skipped). The step with
`id: publish` is skipped; with --publish-to, dist/ is copied there instead.

Used by CI (the samples' workflows must keep working) and by the end-to-end
tests, which deploy the resulting releases with dootd.
"""
import os
import shutil
import subprocess
import sys
import tempfile

import yaml


def main():
    args = sys.argv[1:]
    if not args:
        sys.exit(__doc__)
    app = os.path.abspath(args[0])
    publish_to = None
    if "--publish-to" in args:
        publish_to = os.path.abspath(args[args.index("--publish-to") + 1])
    wf = yaml.safe_load(open(os.path.join(app, ".github/workflows/release.yml")))
    env = dict(os.environ)
    env.update({k: str(v) for k, v in (wf.get("env") or {}).items()})
    env.setdefault("GITHUB_REF_NAME", "v0.0.0-local")
    env["RUNNER_TEMP"] = env.get("RUNNER_TEMP") or tempfile.mkdtemp()
    path_file = tempfile.mktemp()
    open(path_file, "w").close()
    env["GITHUB_PATH"] = path_file

    for job in wf["jobs"].values():
        for step in job["steps"]:
            name = step.get("name") or step.get("uses") or "step"
            if "run" not in step or step.get("id") == "publish":
                print(f"--- skip: {name}", flush=True)
                continue
            print(f"--- {name}", flush=True)
            extra = open(path_file).read().split()
            if extra:
                env["PATH"] = ":".join(extra + [env["PATH"]])
                open(path_file, "w").close()
            step_env = dict(env)
            step_env.update({k: str(v) for k, v in (step.get("env") or {}).items() if "${{" not in str(v)})
            r = subprocess.run(["bash", "-e", "-c", step["run"]], cwd=app, env=step_env)
            if r.returncode != 0:
                sys.exit(f"step {name!r} failed with exit code {r.returncode}")

    if publish_to:
        os.makedirs(publish_to, exist_ok=True)
        for f in os.listdir(os.path.join(app, "dist")):
            shutil.copy(os.path.join(app, "dist", f), publish_to)
        print(f"--- release files copied to {publish_to}")


if __name__ == "__main__":
    main()
