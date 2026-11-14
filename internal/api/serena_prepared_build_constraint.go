package api

import "errors"

const preparedSerenaBuildConstraintName = "build-constraints.txt"
const preparedSerenaBuildConstraintContent = "setuptools<84\n"
const preparedSerenaBuildConstraintRule = "serena-windows-build-constraint-rule-v1:setuptools<84"

// The original attempt tree owns creation and the retained read-only leaf.
// This private value does not grant authority from a pathname or digest.
type preparedSerenaBuildConstraint struct {
	tree *SerenaOwnedTree
	file PreparedSerenaFile
}

func validatePreparedSerenaBuildConstraintReceipt(receipt preparedSerenaUVReceipt) error {
	dependencies := receipt.Tool.BuildConstraintDependencies
	if len(dependencies) != 1 || len(dependencies[0]) != 2 {
		return errors.New("SERENA_BUILD_CONSTRAINT_RECEIPT_MISMATCH")
	}
	name, nameOK := dependencies[0]["name"].(string)
	specifier, specifierOK := dependencies[0]["specifier"].(string)
	if !nameOK || name != "setuptools" || !specifierOK || specifier != "<84" {
		return errors.New("SERENA_BUILD_CONSTRAINT_RECEIPT_MISMATCH")
	}
	return nil
}
