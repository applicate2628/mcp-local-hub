from __future__ import annotations

import pytest


def test_qpc_deadline_is_exact_unchanged_and_never_rebased() -> None:
    from mcphub_em_mcp.cst_saved_field_broker_protocol import (
        BrokerProtocolFailure,
        QpcDeadlineV1,
    )

    deadline = QpcDeadlineV1(10_000_000, 123, 600_000_123)
    assert QpcDeadlineV1.from_wire(deadline.to_wire()) == deadline
    assert deadline.remaining(current_frequency=10_000_000, current_tick=500_000_123) == 10.0
    assert deadline.remaining(current_frequency=10_000_000, current_tick=600_000_123) == 0.0
    with pytest.raises(BrokerProtocolFailure):
        deadline.remaining(current_frequency=9_999_999, current_tick=123)


@pytest.mark.parametrize("field", ["qpc_frequency", "admitted_tick", "deadline_tick"])
def test_qpc_deadline_mutation_is_rejected(field: str) -> None:
    from mcphub_em_mcp.cst_saved_field_broker_protocol import (
        BrokerProtocolFailure,
        QpcDeadlineV1,
    )

    wire = QpcDeadlineV1(100, 20, 6_020).to_wire()
    wire[field] += 1
    with pytest.raises(BrokerProtocolFailure):
        QpcDeadlineV1.from_wire(wire)
