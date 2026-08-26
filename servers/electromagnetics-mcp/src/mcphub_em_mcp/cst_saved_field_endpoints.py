"""Single owner for the fixed CST saved-field local endpoints."""

from types import MappingProxyType

ENROLLMENT_ENDPOINT_V1 = r"\\.\pipe\mcp-local-hub-cst-saved-field-enrollment-v1"
FRONTEND_ENDPOINT_V1 = r"\\.\pipe\mcp-local-hub-cst-saved-field-frontend-v1"
BROKER_ENDPOINT_V1 = r"\\.\pipe\mcp-local-hub-cst-saved-field-v1"
EXACT_ENDPOINTS = (ENROLLMENT_ENDPOINT_V1, FRONTEND_ENDPOINT_V1, BROKER_ENDPOINT_V1)
ENDPOINT_DESCRIPTOR_V1 = MappingProxyType(
    {"enrollment": ENROLLMENT_ENDPOINT_V1, "frontend": FRONTEND_ENDPOINT_V1, "broker": BROKER_ENDPOINT_V1}
)
