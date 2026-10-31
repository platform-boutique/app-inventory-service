package inventory

import (
	"errors"
	"strings"
	"testing"

	pb "inventory-service/proto"
)

func items(kv ...any) []*pb.ReserveItem {
	var out []*pb.ReserveItem
	for i := 0; i < len(kv); i += 2 {
		out = append(out, &pb.ReserveItem{ProductId: kv[i].(string), Quantity: int32(kv[i+1].(int))})
	}
	return out
}

func TestValidateOrderID(t *testing.T) {
	good := []string{"a", "A1", "order-1", "o.r_d-er", "0123", strings.Repeat("a", 64), "  padded  "}
	for _, id := range good {
		if _, err := ValidateOrderID(id); err != nil {
			t.Errorf("ValidateOrderID(%q): %v", id, err)
		}
	}
	if got, _ := ValidateOrderID("  padded  "); got != "padded" {
		t.Errorf("not trimmed: %q", got)
	}
	bad := []string{"", "   ", "-a", ".a", "_a", "a b", "a/b", "a\x00", "ä", strings.Repeat("a", 65), "a\nb"}
	for _, id := range bad {
		_, err := ValidateOrderID(id)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidateOrderID(%q) = %v, want ErrInvalid", id, err)
		}
	}
}

func TestValidateReserve(t *testing.T) {
	long := strings.Repeat("p", 129)
	many := make([]*pb.ReserveItem, 101)
	for i := range many {
		many[i] = &pb.ReserveItem{ProductId: strings.Repeat("x", i+1), Quantity: 1}
	}
	cases := []struct {
		name string
		req  *pb.ReserveRequest
		ok   bool
		want string // substring of the error
	}{
		{"fine", &pb.ReserveRequest{OrderId: "o1", Items: items("b", 2, "a", 1)}, true, ""},
		{"max quantity", &pb.ReserveRequest{OrderId: "o1", Items: items("a", 1000)}, true, ""},
		{"bad order id", &pb.ReserveRequest{OrderId: "-x", Items: items("a", 1)}, false, "order_id"},
		{"no order id", &pb.ReserveRequest{Items: items("a", 1)}, false, "order_id is required"},
		{"no lines", &pb.ReserveRequest{OrderId: "o1"}, false, "at least one line"},
		{"duplicate product", &pb.ReserveRequest{OrderId: "o1", Items: items("a", 1, "a", 2)}, false, "appears twice"},
		{"duplicate after trim", &pb.ReserveRequest{OrderId: "o1", Items: items("a", 1, " a ", 2)}, false, "appears twice"},
		{"quantity 0", &pb.ReserveRequest{OrderId: "o1", Items: items("a", 0)}, false, "quantity"},
		{"quantity negative", &pb.ReserveRequest{OrderId: "o1", Items: items("a", -3)}, false, "quantity"},
		{"quantity 1001", &pb.ReserveRequest{OrderId: "o1", Items: items("a", 1001)}, false, "quantity"},
		{"101 lines", &pb.ReserveRequest{OrderId: "o1", Items: many}, false, "at most 100"},
		{"100 lines", &pb.ReserveRequest{OrderId: "o1", Items: many[:100]}, true, ""},
		{"empty product id", &pb.ReserveRequest{OrderId: "o1", Items: items("", 1)}, false, "product_id"},
		{"product id too long", &pb.ReserveRequest{OrderId: "o1", Items: items(long, 1)}, false, "1-128"},
		{"NUL in product id", &pb.ReserveRequest{OrderId: "o1", Items: items("a\x00b", 1)}, false, "NUL"},
		{"bad utf8", &pb.ReserveRequest{OrderId: "o1", Items: items("a\xffb", 1)}, false, "UTF-8"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := ValidateReserve(c.req)
			if c.ok {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want ErrInvalid containing %q", err, c.want)
			}
		})
	}
}

func TestValidateReserveSortsItems(t *testing.T) {
	_, got, err := ValidateReserve(&pb.ReserveRequest{OrderId: "o1", Items: items("c", 1, "a", 2, "b", 3)})
	if err != nil {
		t.Fatal(err)
	}
	want := []Item{{"a", 2}, {"b", 3}, {"c", 1}}
	if !SameItems(got, want) || got[0].ProductID != "a" || got[2].ProductID != "c" {
		t.Fatalf("got %v, want sorted %v", got, want)
	}
}

func TestValidateStockIDs(t *testing.T) {
	ids, err := ValidateStockIDs([]string{"b", "a", "b", " a "})
	if err != nil || len(ids) != 2 || ids[0] != "b" || ids[1] != "a" {
		t.Fatalf("ids = %v, err = %v (want distinct ids in request order)", ids, err)
	}
	hundred := make([]string, 100)
	for i := range hundred {
		hundred[i] = strings.Repeat("x", i+1)
	}
	if _, err := ValidateStockIDs(hundred); err != nil {
		t.Errorf("100 ids: %v", err)
	}
	for name, in := range map[string][]string{
		"none": nil, "101": append(hundred, "y"), "empty id": {""}, "blank id": {"  "},
		"long id": {strings.Repeat("p", 129)}, "NUL": {"a\x00"},
	} {
		if _, err := ValidateStockIDs(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestSameItems(t *testing.T) {
	a := []Item{{"a", 1}, {"b", 2}}
	cases := []struct {
		name string
		b    []Item
		want bool
	}{
		{"same order", []Item{{"a", 1}, {"b", 2}}, true},
		{"other order", []Item{{"b", 2}, {"a", 1}}, true},
		{"other quantity", []Item{{"a", 1}, {"b", 3}}, false},
		{"other product", []Item{{"a", 1}, {"c", 2}}, false},
		{"fewer", []Item{{"a", 1}}, false},
		{"more", []Item{{"a", 1}, {"b", 2}, {"c", 1}}, false},
		{"duplicate line", []Item{{"a", 1}, {"a", 1}}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		if got := SameItems(a, c.b); got != c.want {
			t.Errorf("%s: SameItems = %v, want %v", c.name, got, c.want)
		}
		if got := SameItems(c.b, a); got != c.want {
			t.Errorf("%s (swapped): SameItems = %v, want %v", c.name, got, c.want)
		}
	}
	if !SameItems(nil, nil) {
		t.Error("two empty lists are the same")
	}
	if SameItems([]Item{{"a", 1}, {"a", 2}}, []Item{{"a", 2}, {"a", 1}}) {
		t.Error("lists with repeated ids never match")
	}
}
