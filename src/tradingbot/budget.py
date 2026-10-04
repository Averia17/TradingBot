"""Bound provider calls and report observed usage, without inventing dollar costs."""

from threading import Lock

from langchain_core.callbacks import BaseCallbackHandler


class CallBudgetExceeded(RuntimeError):
    pass


class CallBudget(BaseCallbackHandler):
    raise_error = True

    def __init__(self, max_calls: int):
        if max_calls <= 0:
            raise ValueError("max_calls must be positive")
        self.max_calls = max_calls
        self._lock = Lock()
        self._calls = 0
        self._input = 0
        self._output = 0
        self._missing = 0
        self._completed = set()

    def on_chat_model_start(self, serialized, messages, *, run_id, **kwargs):
        with self._lock:
            if self._calls >= self.max_calls:
                raise CallBudgetExceeded(f"LLM call budget of {self.max_calls} exhausted")
            self._calls += 1

    def on_llm_end(self, response, *, run_id, **kwargs):
        usage = None
        if response.generations and response.generations[0]:
            message = getattr(response.generations[0][0], "message", None)
            usage = getattr(message, "usage_metadata", None)
        with self._lock:
            if run_id in self._completed:
                return
            self._completed.add(run_id)
            if usage and "input_tokens" in usage and "output_tokens" in usage:
                self._input += usage["input_tokens"]
                self._output += usage["output_tokens"]
            else:
                self._missing += 1

    def summary(self) -> dict:
        with self._lock:
            return {
                "llm_calls": self._calls,
                "input_tokens": self._input,
                "output_tokens": self._output,
                "missing_usage_calls": self._missing,
                "usd_cost": None,  # Model-specific billing ledger is a later milestone.
            }
