"""Local milestone tests must never spend API credits or query market providers."""

import socket

import pytest


@pytest.fixture(autouse=True)
def no_network(monkeypatch):
    def refuse(*args, **kwargs):
        raise OSError("external network is disabled in local research tests")

    monkeypatch.setattr(socket.socket, "connect", refuse)
    monkeypatch.setattr(socket.socket, "connect_ex", refuse)
    monkeypatch.setattr(socket, "getaddrinfo", refuse)
    # yfinance uses native libcurl instead of Python's socket layer.
    from curl_cffi import requests

    monkeypatch.setattr(requests.Session, "request", refuse)
    monkeypatch.setattr(requests.AsyncSession, "request", refuse)
