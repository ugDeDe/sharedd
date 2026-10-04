package server

import "sharedd/registry/internal/globalping"

// Алиасы клиента Globalping: независимая верификация отчётов нод — единственная
// роль пакета (только GET measurement'ов; создание делают сами ноды со своих IP).
type (
	globalpingMeasurement = globalping.Measurement
	GlobalpingChecker     = globalping.GlobalpingChecker
)

var (
	NewGlobalpingChecker       = globalping.New
	evaluateSuccessRatio       = globalping.EvaluateSuccessRatio
	validateMeasurementBinding = globalping.ValidateBinding

	probeResultOK = globalping.ProbeResultOK
)
