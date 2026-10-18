#!/usr/bin/env node
"use strict";

// npm owns delivery and replacement of its native platform payload. The
// command shim resolves that payload directly, and native startup owns Hub
// state reconciliation. Installation does not copy or launch the product.
// Explicit local setup/canonicalize remains a separate user-selected action.
