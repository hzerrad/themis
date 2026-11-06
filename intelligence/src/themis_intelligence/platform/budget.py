"""Token budgets: reserve before a call, settle to what it used."""

from dataclasses import dataclass


class BudgetExceededError(Exception):
    """A reservation would take the budget over its limit."""


@dataclass(frozen=True)
class Reservation:
    tokens: int


class Budget:
    def __init__(self, max_tokens: int, spent: int = 0) -> None:
        self.max_tokens = max_tokens
        # What was spent before, such as by an incident's earlier investigations.
        self.spent = spent
        self._reserved = 0

    def reserve(self, tokens: int) -> Reservation:
        if self.spent + self._reserved + tokens > self.max_tokens:
            raise BudgetExceededError(
                f"{tokens} tokens would exceed the budget of {self.max_tokens} "
                f"({self.spent} spent, {self._reserved} reserved)"
            )
        self._reserved += tokens
        return Reservation(tokens)

    def settle(self, reservation: Reservation, actual: int) -> None:
        self._reserved -= reservation.tokens
        self.spent += actual
