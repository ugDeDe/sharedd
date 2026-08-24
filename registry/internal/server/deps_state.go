package server

import "sharedd/registry/internal/state"

// Алиасы типов и констант state-пакета: исторические короткие имена
// используются по всему серверу и в тестах. Пакет state — единственный
// владелец формата JSON-state-файла; сервер работает с ним через эти имена.
// Новые пакеты (machineapi/webui/…) импортируют state напрямую.
type (
	State            = state.State
	Candidate        = state.Candidate
	Event            = state.Event
	Counters         = state.Counters
	QuarantineState  = state.QuarantineState
	TerminatedRecord = state.TerminatedRecord
	PruneTombstone   = state.PruneTombstone
	DNSOperation     = state.DNSOperation
	SRMDState        = state.SRMDState

	TCPPoint    = state.TCPPoint
	GPPoint     = state.GPPoint
	ReportPoint = state.ReportPoint
	GPProbeLine = state.GPProbeLine
	GPDetail    = state.GPDetail
)

const (
	EventRegistryStarted     = state.EventRegistryStarted
	EventNodeRegistered      = state.EventNodeRegistered
	EventNodeReplaced        = state.EventNodeReplaced
	EventNodeExpired         = state.EventNodeExpired
	EventNodePruned          = state.EventNodePruned
	EventTCPDown             = state.EventTCPDown
	EventTCPUp               = state.EventTCPUp
	EventMetricsDown         = state.EventMetricsDown
	EventMetricsUp           = state.EventMetricsUp
	EventGlobalpingBlocked   = state.EventGlobalpingBlocked
	EventGlobalpingRecovered = state.EventGlobalpingRecovered
	EventQueueJoined         = state.EventQueueJoined
	EventQueueLeft           = state.EventQueueLeft
	EventMasterElected       = state.EventMasterElected
	EventMasterLost          = state.EventMasterLost
	EventDNSUpdated          = state.EventDNSUpdated
	EventDNSError            = state.EventDNSError
	EventDNSDeleted          = state.EventDNSDeleted
	EventConfigChanged       = state.EventConfigChanged
	EventNodeQuarantined     = state.EventNodeQuarantined
	EventQuarantineRecovered = state.EventQuarantineRecovered
	EventNodeTerminated      = state.EventNodeTerminated
	EventBanLifted           = state.EventBanLifted
	EventIPBlocked           = state.EventIPBlocked
	EventSRMDDomainCreated   = state.EventSRMDDomainCreated
	EventSRMDDomainFolded    = state.EventSRMDDomainFolded
	EventSRMDDomainUnfolded  = state.EventSRMDDomainUnfolded
	EventSRMDDomainTaken     = state.EventSRMDDomainTaken
	EventSRMDDomainReleased  = state.EventSRMDDomainReleased

	BanReasonIPBan = state.BanReasonIPBan
	BanReasonDead  = state.BanReasonDead
)
