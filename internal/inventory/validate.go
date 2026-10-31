package inventory

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	pb "inventory-service/proto"
)

// Limits of the API contract (see proto/inventory.proto).
const (
	MaxOrderIDLen   = 64
	MaxProductIDLen = 128
	MaxItems        = 100
	MinQuantity     = 1
	MaxQuantity     = 1000
	MaxStockIDs     = 100
)

// ErrInvalid marks a request that breaks the contract; the gRPC layer maps it to INVALID_ARGUMENT.
var ErrInvalid = errors.New("invalid argument")

type invalidError struct{ msg string }

func (e invalidError) Error() string        { return e.msg }
func (e invalidError) Is(target error) bool { return target == ErrInvalid }

func invalid(format string, args ...any) error {
	return invalidError{msg: fmt.Sprintf(format, args...)}
}

// Item is one reservation line.
type Item struct {
	ProductID string
	Quantity  int32
}

// ValidateOrderID checks the client-supplied id like order-service: 1-64 characters of [A-Za-z0-9._-],
// starting with a letter or digit. Surrounding white space is trimmed.
func ValidateOrderID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", invalid("order_id is required")
	}
	if len(id) > MaxOrderIDLen {
		return "", invalid("order_id must be at most %d characters", MaxOrderIDLen)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum && (i == 0 || (c != '.' && c != '_' && c != '-')) {
			return "", invalid("order_id may only contain letters, digits, '.', '_' and '-' and must start with a letter or digit")
		}
	}
	return id, nil
}

// ValidateProductID trims and checks 1-128 characters of valid UTF-8 without NUL.
func ValidateProductID(field, id string) (string, error) {
	id = strings.TrimSpace(id)
	if !utf8.ValidString(id) || strings.ContainsRune(id, 0) {
		return "", invalid("%s must be valid UTF-8 text without NUL characters", field)
	}
	if n := utf8.RuneCountInString(id); n < 1 || n > MaxProductIDLen {
		return "", invalid("%s must be 1-%d characters, got %d", field, MaxProductIDLen, n)
	}
	return id, nil
}

// ValidateReserve checks a Reserve request and returns the order id and the lines sorted by product id
// (the order in which the store locks the rows).
func ValidateReserve(req *pb.ReserveRequest) (string, []Item, error) {
	orderID, err := ValidateOrderID(req.GetOrderId())
	if err != nil {
		return "", nil, err
	}
	in := req.GetItems()
	if len(in) == 0 {
		return "", nil, invalid("items: at least one line is required")
	}
	if len(in) > MaxItems {
		return "", nil, invalid("items: at most %d lines, got %d", MaxItems, len(in))
	}
	seen := make(map[string]struct{}, len(in))
	items := make([]Item, 0, len(in))
	for i, it := range in {
		pid, err := ValidateProductID(fmt.Sprintf("items[%d].product_id", i), it.GetProductId())
		if err != nil {
			return "", nil, err
		}
		if q := it.GetQuantity(); q < MinQuantity || q > MaxQuantity {
			return "", nil, invalid("items[%d].quantity must be %d-%d, got %d", i, MinQuantity, MaxQuantity, q)
		}
		if _, dup := seen[pid]; dup {
			return "", nil, invalid("items[%d]: product %q appears twice", i, pid)
		}
		seen[pid] = struct{}{}
		items = append(items, Item{ProductID: pid, Quantity: it.GetQuantity()})
	}
	sortItems(items)
	return orderID, items, nil
}

// ValidateStockIDs checks a GetStock request (1-100 ids) and returns the distinct ids in request order.
func ValidateStockIDs(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, invalid("product_ids: at least one id is required")
	}
	if len(ids) > MaxStockIDs {
		return nil, invalid("product_ids: at most %d ids, got %d", MaxStockIDs, len(ids))
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for i, raw := range ids {
		id, err := ValidateProductID(fmt.Sprintf("product_ids[%d]", i), raw)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

func sortItems(items []Item) {
	sort.Slice(items, func(i, j int) bool { return items[i].ProductID < items[j].ProductID })
}

// SameItems reports whether two line lists hold the same quantities per product, whatever their order.
// Lists with a repeated product id never match (a valid request has none).
func SameItems(a, b []Item) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int32, len(a))
	for _, it := range a {
		if _, dup := m[it.ProductID]; dup {
			return false
		}
		m[it.ProductID] = it.Quantity
	}
	seen := make(map[string]struct{}, len(b))
	for _, it := range b {
		if _, dup := seen[it.ProductID]; dup {
			return false
		}
		seen[it.ProductID] = struct{}{}
		if q, ok := m[it.ProductID]; !ok || q != it.Quantity {
			return false
		}
	}
	return true
}
