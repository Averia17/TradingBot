import json
import subprocess

import pytest

from tradingbot import upstream
from tradingbot.upstream import (
    UpstreamError,
    bootstrap,
    latest_release,
    update,
    verify,
)


def git(path, *args):
    return subprocess.check_output(["git", "-C", str(path), *args], text=True).strip()


@pytest.fixture
def project(tmp_path, monkeypatch):
    remote = tmp_path / "official"
    remote.mkdir()
    git(remote, "init", "-q")
    git(remote, "config", "user.email", "fixture@example.invalid")
    git(remote, "config", "user.name", "Fixture")
    for version in ("0.6.0", "0.7.0"):
        (remote / "pyproject.toml").write_text(
            f'[project]\nname = "tradingagents"\nversion = "{version}"\n', encoding="utf-8"
        )
        git(remote, "add", ".")
        git(remote, "commit", "-qm", version)
        git(remote, "tag", "-a", f"v{version}", "-m", version)
    root = tmp_path / "bot"
    root.mkdir()
    (root / "pyproject.toml").write_text(
        '[project]\ndependencies = ["tradingagents==0.6.0"]\n', encoding="utf-8"
    )
    (root / "uv.lock").write_bytes(b"original lock\r\n")
    manifest = {
        "repositories": [
            {
                "name": "trading-agents",
                "url": remote.as_uri(),
                "ref": "v0.6.0",
                "source_commit": git(remote, "rev-parse", "v0.6.0^{commit}"),
                "distribution": "tradingagents",
                "patches": [],
            }
        ],
    }
    (root / "upstream.lock.json").write_text(json.dumps(manifest), encoding="utf-8")
    monkeypatch.setitem(upstream.EXPECTED_URLS, "trading-agents", remote.as_uri())
    bootstrap(root)
    return root, remote


def test_bootstrap_is_clean_detached_exact_and_repeatable(project):
    root, _ = project
    bootstrap(root)
    assert verify(root)[0]["ref"] == "v0.6.0"
    assert not git(root / "vendor/trading-agents", "status", "--porcelain")
    assert git(root / "vendor/trading-agents", "branch", "--show-current") == ""


def test_bootstrap_fetches_tag_when_release_branch_has_the_same_name(tmp_path, monkeypatch):
    remote = tmp_path / "remote"
    remote.mkdir()
    git(remote, "init", "-q")
    git(remote, "config", "user.email", "fixture@example.invalid")
    git(remote, "config", "user.name", "Fixture")
    (remote / "file.txt").write_text("release", encoding="utf-8")
    git(remote, "add", ".")
    git(remote, "commit", "-qm", "tag commit")
    git(remote, "tag", "-a", "v0.6.0", "-m", "release")
    tagged_commit = git(remote, "rev-parse", "HEAD")
    git(remote, "switch", "-c", "v0.6.0")
    (remote / "file.txt").write_text("branch moved", encoding="utf-8")
    git(remote, "commit", "-qam", "branch commit")
    root = tmp_path / "bot"
    root.mkdir()
    monkeypatch.setitem(upstream.EXPECTED_URLS, "trading-agents", remote.as_uri())
    (root / "upstream.lock.json").write_text(
        json.dumps(
            {
                "repositories": [
                    {
                        "name": "trading-agents",
                        "url": remote.as_uri(),
                        "ref": "v0.6.0",
                        "source_commit": tagged_commit,
                        "patches": [],
                    }
                ]
            }
        ),
        encoding="utf-8",
    )
    bootstrap(root)
    assert git(root / "vendor/trading-agents", "rev-parse", "HEAD") == tagged_commit


@pytest.mark.parametrize("has_user_file", [False, True])
def test_bootstrap_resumes_empty_partial_checkout_but_preserves_user_files(project, has_user_file):
    original, remote = project
    root = original.parent / "resume"
    root.mkdir()
    (root / "upstream.lock.json").write_bytes((original / "upstream.lock.json").read_bytes())
    checkout = root / "vendor" / "trading-agents"
    checkout.mkdir(parents=True)
    git(checkout, "init", "-q")
    git(checkout, "remote", "add", "upstream", remote.as_uri())
    if has_user_file:
        (checkout / ".git" / "info" / "exclude").write_text("local.txt\n", encoding="utf-8")
        (checkout / "local.txt").write_bytes(b"local ignored data")
        with pytest.raises(UpstreamError, match="contains files"):
            bootstrap(root)
        assert (checkout / "local.txt").read_bytes() == b"local ignored data"
    else:
        bootstrap(root)
        verify(root)


def test_dirty_checkout_refuses_update_without_discarding_files(project):
    root, _ = project
    checkout = root / "vendor/trading-agents"
    (checkout / "my-work.txt").write_text("do not discard", encoding="utf-8")
    before = git(checkout, "rev-parse", "HEAD")
    with pytest.raises(UpstreamError, match="dirty"):
        update(root, "trading-agents", "v0.7.0", run=lambda *_: None)
    assert git(checkout, "rev-parse", "HEAD") == before
    assert (checkout / "my-work.txt").read_text() == "do not discard"


def test_wrong_remote_and_unlocked_head_refused(project):
    root, remote = project
    checkout = root / "vendor/trading-agents"
    git(checkout, "remote", "set-url", "upstream", "https://example.invalid/repo.git")
    with pytest.raises(UpstreamError, match="remote"):
        verify(root)
    git(checkout, "remote", "set-url", "upstream", remote.as_uri())
    git(checkout, "fetch", "--depth", "1", "upstream", "refs/tags/v0.7.0")
    git(checkout, "switch", "--detach", "FETCH_HEAD^{commit}")
    with pytest.raises(UpstreamError, match="HEAD"):
        verify(root)


def test_success_promotes_annotated_tag_only_after_gates(project):
    root, remote = project
    observed = []

    def run(args, cwd):
        observed.append(args)
        manifest = json.loads((root / "upstream.lock.json").read_text())
        assert manifest["repositories"][0]["ref"] == "v0.6.0"
        if args[1] == "lock":
            (root / "uv.lock").write_bytes(b"candidate lock")

    result = update(root, "trading-agents", "v0.7.0", run=run)
    manifest = json.loads((root / "upstream.lock.json").read_text())
    commit = git(remote, "rev-parse", "v0.7.0^{commit}")
    assert result["status"] == "promoted"
    assert manifest["repositories"][0]["source_commit"] == commit
    assert manifest["repositories"][0]["ref"] == "v0.7.0"
    assert "tradingagents==0.7.0" in (root / "pyproject.toml").read_text()
    assert git(root / "vendor/trading-agents", "rev-parse", "HEAD") == commit
    assert any("pytest" in args for args in observed)
    verify(root)


def test_official_source_cannot_be_replaced_in_manifest(project):
    root, _ = project
    manifest = json.loads((root / "upstream.lock.json").read_text())
    manifest["repositories"][0]["url"] = "https://github.com/example/fork.git"
    (root / "upstream.lock.json").write_text(json.dumps(manifest), encoding="utf-8")
    with pytest.raises(UpstreamError, match="official"):
        verify(root)


def test_ignored_user_file_is_not_overwritten_by_new_tracked_file(project):
    root, remote = project
    (remote / "pyproject.toml").write_text('[project]\nversion="0.8.0"\n', encoding="utf-8")
    (remote / ".gitignore").write_text("local.txt\n", encoding="utf-8")
    git(remote, "add", ".gitignore", "pyproject.toml")
    git(remote, "commit", "-qm", "ignore local files")
    git(remote, "tag", "v0.8.0")
    update(root, "trading-agents", "v0.8.0", run=lambda *_: None)
    checkout = root / "vendor/trading-agents"
    (checkout / "local.txt").write_bytes(b"my private local data")
    (remote / "local.txt").write_bytes(b"new upstream file")
    (remote / "pyproject.toml").write_text('[project]\nversion="0.9.0"\n', encoding="utf-8")
    git(remote, "add", "-f", "local.txt")
    git(remote, "add", "pyproject.toml")
    git(remote, "commit", "-qm", "track local files")
    git(remote, "tag", "v0.9.0")
    with pytest.raises(UpstreamError, match="restored"):
        update(root, "trading-agents", "v0.9.0", run=lambda *_: None)
    assert (checkout / "local.txt").read_bytes() == b"my private local data"
    verify(root)


def test_dynamic_upstream_version_is_read_without_importing_package(project):
    root, remote = project
    (remote / "pyproject.toml").write_text(
        '[project]\nname="tradingagents"\ndynamic=["version"]\n'
        '[tool.setuptools.dynamic]\nversion={attr="tradingagents.__version__"}\n',
        encoding="utf-8",
    )
    package = remote / "tradingagents"
    package.mkdir()
    (package / "__init__.py").write_text(
        '__version__ = "0.8.0"\nraise RuntimeError("must not import package")\n', encoding="utf-8"
    )
    git(remote, "add", ".")
    git(remote, "commit", "-qm", "dynamic version")
    git(remote, "tag", "v0.8.0")
    update(root, "trading-agents", "v0.8.0", run=lambda *_: None)
    assert "tradingagents==0.8.0" in (root / "pyproject.toml").read_text()


def test_expected_commit_refuses_different_candidate(project):
    root, _ = project
    with pytest.raises(UpstreamError, match="expected commit"):
        update(root, "trading-agents", "v0.7.0", expected_commit="a" * 40, run=lambda *_: None)
    verify(root)


@pytest.mark.parametrize("failure", ["lock", "sync", "pytest"])
def test_failure_restores_head_files_and_dependencies(project, failure):
    root, _ = project
    paths = [root / p for p in ("pyproject.toml", "uv.lock", "upstream.lock.json")]
    before = {p: p.read_bytes() for p in paths}
    checkout = root / "vendor/trading-agents"
    head = git(checkout, "rev-parse", "HEAD")
    calls = []
    failed = False

    def run(args, cwd):
        nonlocal failed
        calls.append(args)
        if args[1] == "lock":
            (root / "uv.lock").write_bytes(b"new partially generated lock")
        if failure in args and not failed:
            failed = True
            raise UpstreamError("candidate failed")

    with pytest.raises(UpstreamError, match="restored"):
        update(root, "trading-agents", "v0.7.0", run=run)
    assert {p: p.read_bytes() for p in paths} == before
    assert git(checkout, "rev-parse", "HEAD") == head
    assert calls[-1] == ["uv", "sync", "--frozen", "--extra", "evaluation"]


def test_rollback_install_failure_is_reported(project):
    root, _ = project

    def run(args, cwd):
        raise UpstreamError("install failed")

    with pytest.raises(UpstreamError, match="environment restoration failed"):
        update(root, "trading-agents", "v0.7.0", run=run)
    verify(root)


def test_retagged_same_release_is_refused(project):
    root, remote = project
    git(remote, "tag", "-fa", "v0.6.0", "-m", "retag")
    with pytest.raises(UpstreamError, match="changed commit"):
        update(root, "trading-agents", "v0.6.0", run=lambda *_: None)
    verify(root)


def test_concurrent_update_is_refused(project):
    root, _ = project
    (root / ".upstream-update.lock").write_text("123", encoding="utf-8")
    with pytest.raises(UpstreamError, match="already"):
        update(root, "trading-agents", "v0.7.0", run=lambda *_: None)


@pytest.mark.parametrize("tag", ["main", "v0.7.0rc1", "../../bad", "v0.7.0;whoami"])
def test_only_stable_version_tags_can_be_promoted(project, tag):
    root, _ = project
    with pytest.raises(UpstreamError, match="stable"):
        update(root, "trading-agents", tag, run=lambda *_: None)


@pytest.mark.parametrize("field", ["prerelease", "draft"])
def test_latest_release_rejects_nonstable_api_result(field):
    repo = {"url": "https://github.com/TauricResearch/TradingAgents.git"}
    with pytest.raises(UpstreamError, match="stable"):
        latest_release(repo, fetch=lambda _: {"tag_name": "v0.7.0", field: True})


def test_latest_release_uses_official_endpoint():
    observed = []

    def fetch(url):
        observed.append(url)
        return {"tag_name": "v0.7.0", "draft": False, "prerelease": False}

    assert (
        latest_release({"url": "https://github.com/TauricResearch/TradingAgents.git"}, fetch)
        == "v0.7.0"
    )
    assert observed == ["https://api.github.com/repos/TauricResearch/TradingAgents/releases/latest"]
