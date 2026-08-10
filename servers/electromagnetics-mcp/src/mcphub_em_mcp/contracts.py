from __future__ import annotations

from typing import Annotated, Literal

from pydantic import Field

SolveAction = Annotated[
    Literal["start", "status", "result", "cancel", "preflight"],
    Field(
        description=(
            "start submits a validated job; status/result/cancel require job_id; "
            "preflight performs complete validation without launching the solver"
        )
    ),
]
PassCount = Annotated[int, Field(ge=1, le=100)]
PortModeChecks = Annotated[int, Field(ge=1, le=20)]
PositiveFiniteFloat = Annotated[float, Field(gt=0, allow_inf_nan=False)]
ExclusiveUnitFloat = Annotated[float, Field(gt=0, lt=1, allow_inf_nan=False)]
LicenseWaitSeconds = Annotated[float, Field(ge=0, le=86400, allow_inf_nan=False)]
LicensePollSeconds = Annotated[float, Field(ge=0.1, le=3600, allow_inf_nan=False)]
PositiveTimeoutSeconds = Annotated[float, Field(gt=0, allow_inf_nan=False)]
HFSSBasisOrder = Literal[-1, 0, 1, 2]
CSTTetOrder = Literal["First", "Second"]
FrequencyRangeGHz = Annotated[list[PositiveFiniteFloat], Field(min_length=2, max_length=2)]
FrequencySamplesGHz = Annotated[list[PositiveFiniteFloat], Field(min_length=1, max_length=10000)]
