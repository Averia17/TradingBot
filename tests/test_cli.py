import json

from tradingbot.cli import main


def test_offline_demo_writes_json_schema_evidence_and_research_only_intents(tmp_path):
    output = tmp_path / "demo"
    assert main(["demo", "--output", str(output)]) == 0
    result = json.loads((output / "intents.json").read_text(encoding="utf-8"))
    metadata = json.loads((output / "metadata.json").read_text(encoding="utf-8"))
    assert result["execution_mode"] == "research"
    assert {i["symbol"] for i in result["intents"]} == {"TSLA", "NVDA"}
    assert all(i["action"] == "HOLD" and i["notional_usd"] == "0" for i in result["intents"])
    assert metadata["source_mode"] == "synthetic_fixture"
    assert metadata["llm_calls"] == 0
    assert (output / "reports.json").exists()


def test_completed_run_is_not_overwritten(tmp_path):
    output = tmp_path / "run"
    main(["demo", "--output", str(output)])
    before = (output / "intents.json").read_bytes()
    assert main(["demo", "--output", str(output)]) == 2
    assert (output / "intents.json").read_bytes() == before


def test_schema_export_uses_same_contract_as_adapter(tmp_path):
    output = tmp_path / "intent.schema.json"
    assert main(["schema", "--output", str(output)]) == 0
    schema = json.loads(output.read_text(encoding="utf-8"))
    assert schema["additionalProperties"] is False
    assert schema["properties"]["execution_mode"]["const"] == "research"
    money = schema["$defs"]["TradeIntent"]["properties"]["notional_usd"]
    assert money["type"] == "string"
    assert "pattern" in money


def test_invalid_portfolio_fails_before_provider_or_market_call(tmp_path):
    book = tmp_path / "bad.json"
    book.write_text('{"cash_usd": "-1"}', encoding="utf-8")
    assert (
        main(
            [
                "analyze",
                "--portfolio",
                str(book),
                "--symbols",
                "TSLA",
                "--output",
                str(tmp_path / "run"),
            ]
        )
        == 2
    )
    assert not (tmp_path / "run").exists()


def test_doctor_exposes_actual_adapter_compatibility(capsys):
    assert main(["doctor"]) == 0
    components = json.loads(capsys.readouterr().out)
    assert components["tradingagents_adapter"]["compatible"] is True


def test_doctor_identifies_an_incompatible_upstream_without_sensitive_config(monkeypatch, capsys):
    from tradingbot.adapters import tradingagents

    def incompatible():
        raise tradingagents.UpstreamCompatibilityError("TradingAgents propagate/portfolio changed")

    monkeypatch.setattr(tradingagents, "check_compatibility", incompatible)
    assert main(["doctor"]) == 2
    assert "propagate/portfolio changed" in capsys.readouterr().err
