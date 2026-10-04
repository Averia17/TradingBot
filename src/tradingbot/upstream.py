"""Clean upstream checkouts and tested release promotion; never used during trading."""

import argparse
import ast
import contextlib
import json
import os
import re
import subprocess
import sys
import tomllib
import urllib.error
import urllib.request
from collections.abc import Callable
from datetime import UTC, datetime
from pathlib import Path
from uuid import uuid4

SUPPORTED = {"trading-agents": "tradingagents", "nautilus-trader": "nautilus_trader"}
EXPECTED_URLS = {
    "trading-agents": "https://github.com/TauricResearch/TradingAgents.git",
    "nautilus-trader": "https://github.com/nautechsystems/nautilus_trader.git",
}
STABLE_TAG = re.compile(r"v?(\d+\.\d+\.\d+)")
Runner = Callable[[list[str], Path], None]


class UpstreamError(RuntimeError):
    pass


def _git(checkout: Path, *args: str, missing_ok: bool = False) -> str:
    result = subprocess.run(
        ["git", "-C", str(checkout), *args], capture_output=True, text=True, check=False
    )
    if missing_ok and result.returncode == 1:
        return ""
    if result.returncode:
        raise UpstreamError(f"Git operation failed ({' '.join(args)}): {result.stderr.strip()}")
    return result.stdout.strip()


def _run(args: list[str], cwd: Path) -> None:
    print(f"Running: {' '.join(args)}", flush=True)
    result = subprocess.run(args, cwd=cwd, check=False)
    if result.returncode:
        raise UpstreamError(f"Check failed: {' '.join(args)} (exit {result.returncode})")


def _manifest(root: Path) -> dict:
    manifest = json.loads((root / "upstream.lock.json").read_text(encoding="utf-8"))
    names = set()
    for repo in manifest["repositories"]:
        name = repo["name"]
        if name not in SUPPORTED or name in names:
            raise UpstreamError(f"Unsupported or duplicate repository: {name}")
        names.add(name)
        if repo["url"] != EXPECTED_URLS[name]:
            raise UpstreamError(f"Expected official source URL for {name}")
        _version(repo["ref"])
        if not re.fullmatch(r"[0-9a-f]{40}", repo["source_commit"]):
            raise UpstreamError(f"Invalid source SHA: {name}")
        if repo.get("patches"):
            raise UpstreamError(f"Clean upstream policy forbids vendor patches: {name}")
        if repo.get("distribution", SUPPORTED[name]) != SUPPORTED[name]:
            raise UpstreamError(f"Unexpected distribution: {name}")
    return manifest


def _version(tag: str) -> str:
    match = STABLE_TAG.fullmatch(tag)
    if not match:
        raise UpstreamError(f"Expected a stable version tag, received {tag!r}")
    return match.group(1)


def _checkout(root: Path, repo: dict) -> Path:
    checkout = root / "vendor" / repo["name"]
    # Refuse symlinks/junctions escaping this project's vendor directory.
    if not checkout.resolve().is_relative_to((root / "vendor").resolve()):
        raise UpstreamError(f"Checkout escapes vendor directory: {repo['name']}")
    return checkout


def _verify_clean_checkout(root: Path, repo: dict) -> Path:
    checkout = _checkout(root, repo)
    if not checkout.exists():
        raise UpstreamError(f"Missing checkout: {repo['name']}; run bootstrap")
    if Path(_git(checkout, "rev-parse", "--show-toplevel")).resolve() != checkout.resolve():
        raise UpstreamError(f"Expected independent upstream checkout: {repo['name']}")
    if _git(checkout, "remote", "get-url", "--all", "upstream").splitlines() != [repo["url"]]:
        raise UpstreamError(f"Unexpected upstream remote: {repo['name']}")
    if _git(checkout, "status", "--porcelain", "--untracked-files=all"):
        raise UpstreamError(f"Refusing dirty upstream checkout: {repo['name']}")
    return checkout


def _verify_repo(root: Path, repo: dict) -> None:
    checkout = _verify_clean_checkout(root, repo)
    if _git(checkout, "rev-parse", "HEAD") != repo["source_commit"]:
        raise UpstreamError(f"HEAD differs from approved lock: {repo['name']}; no reset performed")


def verify(root: Path) -> list[dict]:
    manifest = _manifest(root)
    for repo in manifest["repositories"]:
        _verify_repo(root, repo)
    return manifest["repositories"]


@contextlib.contextmanager
def _exclusive(root: Path):
    path = root / ".upstream-update.lock"
    try:
        fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    except FileExistsError as exc:
        raise UpstreamError(
            "An upstream operation is already active; inspect .upstream-update.lock"
        ) from exc
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            handle.write(str(os.getpid()))
        yield
    finally:
        path.unlink()


def bootstrap(root: Path) -> None:
    with _exclusive(root):
        manifest = _manifest(root)
        for repo in manifest["repositories"]:
            checkout = _checkout(root, repo)
            if not checkout.exists():
                checkout.parent.mkdir(parents=True, exist_ok=True)
                # init + exact-tag fetch avoids clone --branch ambiguity when
                # an upstream release branch and tag have the same name.
                _run(["git", "init", str(checkout)], root)
                _git(checkout, "remote", "add", "upstream", repo["url"])
            _verify_clean_checkout(root, repo)
            if not _git(checkout, "rev-parse", "--verify", "--quiet", "HEAD", missing_ok=True):
                # A failed first fetch is retryable only while checkout has no
                # files at all (including ignored files) and no commit/index edits.
                if any(path.name != ".git" for path in checkout.iterdir()):
                    raise UpstreamError("Incomplete checkout contains files; refusing to overwrite")
                _git(
                    checkout,
                    "fetch",
                    "--depth",
                    "1",
                    "--no-tags",
                    "upstream",
                    f"refs/tags/{repo['ref']}",
                )
                if _git(checkout, "rev-parse", "FETCH_HEAD^{commit}") != repo["source_commit"]:
                    raise UpstreamError(
                        f"Release changed commit: {repo['name']}; refusing installation"
                    )
                _git(checkout, "switch", "--no-overwrite-ignore", "--detach", repo["source_commit"])
            _verify_repo(root, repo)


def _fetch_json(url: str) -> dict:
    request = urllib.request.Request(
        url,
        headers={
            "User-Agent": "TradingBot-upstream-check",
            "Accept": "application/vnd.github+json",
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)
    except (urllib.error.URLError, json.JSONDecodeError) as exc:
        raise UpstreamError(f"Cannot query official releases at {url}: {exc}") from exc


def latest_release(repo: dict, fetch: Callable = _fetch_json) -> str:
    match = re.fullmatch(r"https://github\.com/([\w.-]+)/([\w.-]+)\.git", repo["url"])
    if not match:
        raise UpstreamError("Latest release discovery requires an official GitHub URL")
    release = fetch(
        f"https://api.github.com/repos/{match.group(1)}/{match.group(2)}/releases/latest"
    )
    if release.get("draft") or release.get("prerelease"):
        raise UpstreamError("Latest release API did not return a stable release")
    tag = release.get("tag_name", "")
    _version(tag)
    return tag


def _candidate(root: Path, repo: dict, tag: str, expected_commit: str | None = None) -> str:
    _version(tag)
    if expected_commit is not None and not re.fullmatch(r"[0-9a-f]{40}", expected_commit):
        raise UpstreamError("Invalid expected commit SHA")
    checkout = _checkout(root, repo)
    _git(checkout, "fetch", "--depth", "1", "--no-tags", "upstream", f"refs/tags/{tag}")
    candidate = _git(checkout, "rev-parse", "FETCH_HEAD^{commit}")
    if expected_commit is not None and candidate != expected_commit:
        raise UpstreamError("Candidate differs from expected commit; refusing changed release")
    if tag == repo["ref"] and candidate != repo["source_commit"]:
        raise UpstreamError(f"Release changed commit: {tag}; refusing a retagged release")
    return candidate


def _package_version(checkout: Path, commit: str) -> str:
    metadata = tomllib.loads(_git(checkout, "show", f"{commit}:pyproject.toml"))
    project = metadata["project"]
    if "version" in project:
        return project["version"]
    # Actual TradingAgents v0.6.0 uses setuptools dynamic attr metadata.
    # Read its constant via AST: importing upstream here could load user .env or
    # execute changed code before the candidate compatibility gate.
    declaration = metadata.get("tool", {}).get("setuptools", {}).get("dynamic", {})
    if "version" in project.get("dynamic", []) and declaration.get("version") == {
        "attr": "tradingagents.__version__"
    }:
        tree = ast.parse(_git(checkout, "show", f"{commit}:tradingagents/__init__.py"))
        for node in tree.body:
            if isinstance(node, ast.Assign) and any(
                isinstance(target, ast.Name) and target.id == "__version__"
                for target in node.targets
            ):
                if isinstance(node.value, ast.Constant) and isinstance(node.value.value, str):
                    return node.value.value
    raise UpstreamError("TradingAgents version metadata changed; update metadata adapter")


def _pin_dependency(root: Path, repo: dict, version: str) -> None:
    path = root / "pyproject.toml"
    data = path.read_bytes()
    name = SUPPORTED[repo["name"]].encode("ascii")
    # Only our exact requirement is changed; other user edits stay byte-for-byte intact.
    pattern = rb'(["\x27]' + re.escape(name) + rb'==)\d+\.\d+\.\d+(["\x27])'
    data, count = re.subn(pattern, lambda m: m[1] + version.encode("ascii") + m[2], data)
    if count != 1:
        raise UpstreamError(f"Expected one exact version requirement for {name.decode()}")
    path.write_bytes(data)


def _checks(root: Path, run: Runner) -> None:
    prefix = ["uv", "run", "--frozen", "--extra", "evaluation"]
    run(["uv", "pip", "check"], root)
    run([*prefix, "tradingbot", "doctor"], root)
    run([*prefix, "ruff", "check", "src", "tests", "scripts"], root)
    run([*prefix, "pytest", "tests", "--junitxml=runtime/verification/ours.xml"], root)
    run(["uv", "build", "--no-sources"], root)


def _report(root: Path, report: dict) -> dict:
    destination = root / "runtime" / "upstream-updates"
    destination.mkdir(parents=True, exist_ok=True)
    report["checked_at"] = datetime.now(UTC).isoformat()
    path = destination / f"{uuid4()}.json"
    path.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    report["report_path"] = str(path)
    return report


def update(
    root: Path,
    name: str,
    tag: str | None = None,
    *,
    expected_commit: str | None = None,
    run: Runner = _run,
) -> dict:
    """Promote only after tests. On normal failure restore source, files and installed environment."""
    with _exclusive(root):
        manifest = _manifest(root)
        for entry in manifest["repositories"]:
            _verify_repo(root, entry)
        repo = next((r for r in manifest["repositories"] if r["name"] == name), None)
        if repo is None:
            raise UpstreamError(f"Unknown repository: {name}")
        tag = tag or latest_release(repo)
        version = _version(tag)
        checkout = _checkout(root, repo)
        candidate = _candidate(root, repo, tag, expected_commit)
        if name == "trading-agents":
            if _package_version(checkout, candidate) != version:
                raise UpstreamError("Release tag and TradingAgents package version disagree")
        paths = [root / p for p in ("pyproject.toml", "uv.lock", "upstream.lock.json")]
        snapshots = {p: p.read_bytes() for p in paths}
        previous_head = repo["source_commit"]
        previous_branch = _git(checkout, "branch", "--show-current")
        try:
            _git(checkout, "switch", "--no-overwrite-ignore", "--detach", candidate)
            _pin_dependency(root, repo, version)
            run(["uv", "lock", "--upgrade-package", SUPPORTED[name]], root)
            run(["uv", "sync", "--frozen", "--extra", "evaluation"], root)
            _checks(root, run)
            # Gate must not leave source modifications behind.
            if _git(checkout, "status", "--porcelain", "--untracked-files=all"):
                raise UpstreamError("Compatibility gate left dirty upstream source")
            repo.update(ref=tag, source_commit=candidate, distribution=SUPPORTED[name], patches=[])
            manifest["checked_at"] = datetime.now(UTC).date().isoformat()
            (root / "upstream.lock.json").write_text(
                json.dumps(manifest, indent=2) + "\n", encoding="utf-8"
            )
            return _report(
                root,
                {
                    "status": "promoted",
                    "repository": name,
                    "ref": tag,
                    "source_commit": candidate,
                    "previous_commit": previous_head,
                    "checks": ["dependency metadata", "doctor", "ruff", "pytest", "build"],
                },
            )
        except (Exception, KeyboardInterrupt) as exc:
            restoration_errors = []
            for path, data in snapshots.items():
                try:
                    path.write_bytes(data)
                except OSError as restore_exc:
                    restoration_errors.append(str(restore_exc))
            try:
                _git(
                    checkout,
                    "switch",
                    "--no-overwrite-ignore",
                    *([previous_branch] if previous_branch else ["--detach", previous_head]),
                )
            except UpstreamError as restore_exc:
                restoration_errors.append(str(restore_exc))
            try:
                run(["uv", "sync", "--frozen", "--extra", "evaluation"], root)
            except Exception as restore_exc:
                restoration_errors.append(str(restore_exc))
            if restoration_errors:
                raise UpstreamError(
                    "Candidate failed; environment restoration failed: "
                    + "; ".join(restoration_errors)
                ) from exc
            raise UpstreamError(
                f"Candidate failed; previous HEAD, files and environment restored: {exc}"
            ) from exc


def main(argv: list[str] | None = None, *, root: Path | None = None) -> int:
    root = root or Path(__file__).resolve().parents[2]
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["bootstrap", "verify", "check", "update"])
    parser.add_argument("--repo", choices=list(SUPPORTED), default="trading-agents")
    parser.add_argument(
        "--ref", help="Explicit stable release tag; otherwise latest GitHub release"
    )
    parser.add_argument("--expected-commit", help="Require this exact candidate SHA (CI)")
    args = parser.parse_args(argv)
    try:
        if args.command == "bootstrap":
            bootstrap(root)
            result = {"status": "bootstrapped", "repositories": verify(root)}
        elif args.command == "verify":
            result = {"status": "verified", "repositories": verify(root)}
        elif args.command == "check":
            with _exclusive(root):
                repos = verify(root)
                repo = next(r for r in repos if r["name"] == args.repo)
                latest = args.ref or latest_release(repo)
                commit = _candidate(root, repo, latest, args.expected_commit)
                result = {
                    "repository": args.repo,
                    "current": repo["ref"],
                    "latest": latest,
                    "candidate_commit": commit,
                    "update_available": latest != repo["ref"],
                }
        else:
            result = update(root, args.repo, args.ref, expected_commit=args.expected_commit)
        print(json.dumps(result, indent=2))
        return 0
    except (UpstreamError, OSError, ValueError, KeyError, StopIteration) as exc:
        print(f"Upstream operation refused: {exc}", file=sys.stderr)
        return 2
