// Code generated by mockery v1.0.0. DO NOT EDIT.

package automock

import (
	mock "github.com/stretchr/testify/mock"

	v1alpha2 "github.com/kyma-project/kyma/components/function-controller/pkg/apis/serverless/v1alpha2"
)

// StatsCollector is an autogenerated mock type for the StatsCollector type
type StatsCollector struct {
	mock.Mock
}

// UpdateReconcileStats provides a mock function with given fields: f, cond
func (_m *StatsCollector) UpdateReconcileStats(f *v1alpha2.Function, cond v1alpha2.Condition) {
	_m.Called(f, cond)
}
