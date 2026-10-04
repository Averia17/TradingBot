from decimal import Decimal

from tradingbot.evaluation import run_synthetic_probe


def test_selected_native_engine_tracks_two_simulated_equities_and_cash():
    result = run_synthetic_probe()
    assert result["source_mode"] == "synthetic_fixture"
    assert result["alpha_measured"] is False
    assert result["starting_cash_usd"] == "1000.00"
    assert Decimal(result["ending_cash_usd"]) == Decimal("700")
    assert result["open_positions"] == {"TSLA": "1", "NVDA": "2"}
    assert result["filled_orders"] == 2
