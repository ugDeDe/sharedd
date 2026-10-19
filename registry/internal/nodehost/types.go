package nodehost

import "encoding/json"

type Balance struct {
	Amount   Decimal `json:"amount"`
	Currency string  `json:"currency"`
	Scope    string  `json:"scope"`
}

type Account struct {
	ID       string     `json:"id"`
	Email    string     `json:"email"`
	Username string     `json:"username"`
	Balance  Decimal    `json:"balance"`
	API      AccountAPI `json:"api"`
}

type AccountAPI struct {
	Test         bool   `json:"test"`
	Mode         string `json:"mode"`
	Tier         string `json:"tier"`
	MaxServers   int    `json:"maxServers"`
	RateLimitRPM int    `json:"rateLimitRpm"`
}

type Location struct {
	Name        string  `json:"name"`
	Code        string  `json:"code"`
	Flag        string  `json:"flag"`
	PriceMarkup Decimal `json:"priceMarkup"`
}
type OSTemplate struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	// Title optional: live catalog entries carry {id,name} only (2026-10-17).
	Title string `json:"title,omitempty"`
}
type NetworkOptions struct {
	Speeds []string `json:"speeds"`
	// HasAvailableNodes — live key is hasAvailableNodes (2026-10-17);
	// prelaunch docs said "available", which always parsed false.
	HasAvailableNodes bool `json:"hasAvailableNodes"`
}
type Range struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// DiskType — live /v1/catalog/disk-types entries are objects, not strings.
type DiskType struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Limits matches the live /v1/catalog/limits shape (2026-10-17):
// {"types":{"VPS":true,...},"limits":{"cpu":{min,max},"ram":{...},"disk":{...}}}.
type Limits struct {
	Types  map[string]bool `json:"types"`
	Limits struct {
		CPU  Range `json:"cpu"`
		RAM  Range `json:"ram"`
		Disk Range `json:"disk"`
	} `json:"limits"`
}

// EstimateRequest carries only the estimate-accepted fields: sending
// name/cpu/diskType/os here is a live 400 ("property X should not exist") —
// those belong to CreateRequest only.
type EstimateRequest struct {
	Cores        int    `json:"cores"`
	RAM          int    `json:"ram"`
	Disk         int    `json:"disk"`
	NetworkSpeed string `json:"networkSpeed,omitempty"`
	Period       string `json:"period,omitempty"`
	Location     string `json:"location,omitempty"`
}
type Estimate struct {
	MonthlyPrice Decimal `json:"monthlyPrice"`
	PeriodPrice  Decimal `json:"periodPrice"`
	PeriodDays   int     `json:"periodDays"`
	Currency     string  `json:"currency"`
}
type CreateRequest struct {
	Name         string `json:"name"`
	CPU          string `json:"cpu"`
	Cores        int    `json:"cores"`
	RAM          int    `json:"ram"`
	Disk         int    `json:"disk"`
	DiskType     string `json:"diskType"`
	OS           string `json:"os"`
	Location     string `json:"location"`
	Period       string `json:"period,omitempty"`
	NetworkSpeed string `json:"networkSpeed,omitempty"`
	PromoCode    string `json:"promoCode,omitempty"`
}
type CreateResult struct {
	OperationID string `json:"operationId"`
	Status      string `json:"status"`
	Type        string `json:"type"`
}

// OperationError retains only locally allowlisted codes, never provider text.
type OperationError struct {
	Code string `json:"code"`
}

func (e *OperationError) UnmarshalJSON(data []byte) error {
	var message string
	if err := json.Unmarshal(data, &message); err == nil {
		e.Code = boundCode(message)
		return nil
	}
	var wire struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return failure("invalid_response")
	}
	e.Code = boundCode(wire.Code)
	if e.Code == "provider_error" && boundCode(wire.Message) == "insufficient_funds" {
		e.Code = "insufficient_funds"
	}
	return nil
}

type Operation struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Status       string          `json:"status"`
	ResourceType string          `json:"resourceType"`
	ResourceID   string          `json:"resourceId"`
	Error        *OperationError `json:"error"`
}
type Server struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	IP        string `json:"ip"`
	Cores     int    `json:"cores"`
	RAM       int    `json:"ram"`
	Disk      int    `json:"disk"`
	OS        string `json:"os"`
	Domain    string `json:"domain"`
	Location  string `json:"location"`
	AutoRenew bool   `json:"autoRenew"`
	ExpiresAt string `json:"expiresAt"`
	// Secret for a future credential-store worker, never a cabinet DTO/log value.
	Password string `json:"-"`
}

func (s *Server) UnmarshalJSON(data []byte) error {
	type public Server
	var wire struct {
		public
		Password string `json:"password"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*s = Server(wire.public)
	s.Password = wire.Password
	return nil
}

type ServerPage struct {
	Data       []Server `json:"data"`
	NextCursor *string  `json:"nextCursor"`
}
type DeleteResult struct {
	Refund Decimal `json:"refund"`
}
type Subnet struct {
	RangeID int    `json:"rangeId"`
	Name    string `json:"name"`
	Free    int    `json:"free"`
}
type ChangeIPResult struct {
	IP      string  `json:"ip"`
	Charged Decimal `json:"charged"`
}
