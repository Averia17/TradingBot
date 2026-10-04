"""Export serialized contracts and a deterministic Python-produced wire fixture."""

import json
from datetime import UTC, datetime, timedelta
from pathlib import Path
from uuid import UUID

from tradingbot.contracts import DecisionBatch, PortfolioSnapshot
from tradingbot.demo import SyntheticDecisionModel, SyntheticGraph
from tradingbot.research import collect_reports, create_decisions

ROOT = Path(__file__).resolve().parents[1]


def export():
    target = ROOT / "shared" / "trade-intent"
    consumer = ROOT / "go" / "internal" / "contract"
    consumer.mkdir(parents=True, exist_ok=True)
    for name, model in (("schema", DecisionBatch), ("portfolio.schema", PortfolioSnapshot)):
        content = json.dumps(model.model_json_schema(mode="serialization"), indent=2) + "\n"
        (target / f"{name}.json").write_text(content, encoding="utf-8")
        (consumer / f"{name}.json").write_text(content, encoding="utf-8")
    at = datetime(2026, 10, 4, 12, tzinfo=UTC)
    book = PortfolioSnapshot(snapshot_id=UUID(int=1), as_of=at, cash_usd="1000")
    reports = collect_reports(SyntheticGraph(), ["TSLA", "NVDA"], "2026-10-04", book)
    batch = create_decisions(SyntheticDecisionModel(["TSLA", "NVDA"]), reports, book)
    batch = DecisionBatch.model_validate(batch.model_dump() | {
        "run_id": UUID(int=2), "created_at": at, "expires_at": at + timedelta(minutes=30),
        "intents": tuple(i.model_copy(update={"decision_id": UUID(int=n + 3)})
                         for n, i in enumerate(batch.intents)),
    })
    for name, value in (("valid-batch", batch), ("valid-portfolio", book)):
        (target / f"{name}.json").write_text(value.model_dump_json(indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    export()
