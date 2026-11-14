package inventory

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	pb "inventory-service/proto"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Defaults.
const (
	DefaultReservationTTL = 15 * time.Minute
	DefaultSweepEvery     = time.Minute

	sweepBatch   = 200
	sweepTimeout = 30 * time.Second
)

// Config configures a Service.
type Config struct {
	ReservationTTL time.Duration // 0 = DefaultReservationTTL
	SweepEvery     time.Duration // 0 = DefaultSweepEvery
	Now            func() time.Time
}

// Service implements the gRPC InventoryService.
type Service struct {
	pb.UnimplementedInventoryServiceServer
	store      Store
	log        *logrus.Logger
	ttl        time.Duration
	sweepEvery time.Duration
	now        func() time.Time
	ready      atomic.Bool
	bg         sync.WaitGroup
}

// NewService creates the service. It answers UNAVAILABLE until SetReady(true).
func NewService(store Store, log *logrus.Logger, cfg Config) *Service {
	if cfg.ReservationTTL <= 0 {
		cfg.ReservationTTL = DefaultReservationTTL
	}
	if cfg.SweepEvery <= 0 {
		cfg.SweepEvery = DefaultSweepEvery
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{store: store, log: log, ttl: cfg.ReservationTTL, sweepEvery: cfg.SweepEvery, now: cfg.Now}
}

// SetReady switches the handlers (and the sweep) on or off.
func (s *Service) SetReady(ok bool) { s.ready.Store(ok) }

// Wait blocks until the sweeper started by RunSweeper has stopped (tests, shutdown).
func (s *Service) Wait() { s.bg.Wait() }

func (s *Service) checkReady() error {
	if !s.ready.Load() {
		return status.Error(codes.Unavailable, "database not ready")
	}
	return nil
}

// clock returns the current time in UTC, cut to Postgres' microsecond precision so that a stored value
// and the one used for a comparison are identical.
func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// toStatus maps internal errors to gRPC status codes without leaking SQL details to the caller.
func (s *Service) toStatus(ctx context.Context, method string, err error) error {
	switch {
	case errors.Is(err, ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrNotFound):
		return status.Error(codes.NotFound, "no reservation for this order_id")
	case errors.Is(err, ErrItemsDiffer):
		return status.Error(codes.AlreadyExists, "this order_id is already reserved with different items")
	case errors.Is(err, ErrReleased):
		return status.Error(codes.FailedPrecondition, "the reservation was released or has expired")
	case errors.Is(err, ErrCommitted):
		return status.Error(codes.FailedPrecondition, "the reservation is committed: sold stock is not returned")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	case errors.Is(err, ErrUnavailable):
		s.log.WithContext(ctx).WithError(err).Warnf("[%s] database unavailable", method)
		return status.Error(codes.Unavailable, "database unavailable")
	default:
		s.log.WithContext(ctx).WithError(err).Errorf("[%s] error", method)
		return status.Error(codes.Internal, "internal error")
	}
}

// GetStock returns the levels of the known products among the requested ones.
func (s *Service) GetStock(ctx context.Context, req *pb.GetStockRequest) (*pb.GetStockResponse, error) {
	if err := s.checkReady(); err != nil {
		return nil, err
	}
	ids, err := ValidateStockIDs(req.GetProductIds())
	if err != nil {
		return nil, s.toStatus(ctx, "GetStock", err)
	}
	levels, err := s.store.GetStock(ctx, ids, s.clock())
	if err != nil {
		return nil, s.toStatus(ctx, "GetStock", err)
	}
	out := make([]*pb.StockLevel, len(levels))
	for i, l := range levels {
		out[i] = &pb.StockLevel{ProductId: l.ProductID, Available: l.Available}
	}
	return &pb.GetStockResponse{Levels: out}, nil
}

// Reserve holds the quantities for an order, all or nothing.
func (s *Service) Reserve(ctx context.Context, req *pb.ReserveRequest) (*pb.ReserveResponse, error) {
	if err := s.checkReady(); err != nil {
		return nil, err
	}
	orderID, items, err := ValidateReserve(req)
	if err != nil {
		return nil, s.toStatus(ctx, "Reserve", err)
	}
	res, err := s.store.Reserve(ctx, orderID, items, s.clock(), s.ttl)
	if err != nil {
		return nil, s.toStatus(ctx, "Reserve", err)
	}
	s.log.WithContext(ctx).WithFields(logrus.Fields{"order_id": orderID, "lines": len(items), "reserved": res.Reserved, "shortages": len(res.Shortages)}).Info("reserve")
	out := &pb.ReserveResponse{Reserved: res.Reserved}
	for _, sh := range res.Shortages {
		out.Shortages = append(out.Shortages, &pb.Shortage{ProductId: sh.ProductID, Requested: sh.Requested, Available: sh.Available})
	}
	return out, nil
}

// Commit turns a RESERVED reservation into COMMITTED.
func (s *Service) Commit(ctx context.Context, req *pb.CommitRequest) (*pb.CommitResponse, error) {
	if err := s.checkReady(); err != nil {
		return nil, err
	}
	orderID, err := ValidateOrderID(req.GetOrderId())
	if err != nil {
		return nil, s.toStatus(ctx, "Commit", err)
	}
	if err := s.store.Commit(ctx, orderID, s.clock()); err != nil {
		return nil, s.toStatus(ctx, "Commit", err)
	}
	s.log.WithContext(ctx).WithField("order_id", orderID).Info("commit")
	return &pb.CommitResponse{}, nil
}

// Release gives the held quantities back.
func (s *Service) Release(ctx context.Context, req *pb.ReleaseRequest) (*pb.ReleaseResponse, error) {
	if err := s.checkReady(); err != nil {
		return nil, err
	}
	orderID, err := ValidateOrderID(req.GetOrderId())
	if err != nil {
		return nil, s.toStatus(ctx, "Release", err)
	}
	if err := s.store.Release(ctx, orderID, s.clock()); err != nil {
		return nil, s.toStatus(ctx, "Release", err)
	}
	s.log.WithContext(ctx).WithField("order_id", orderID).Info("release")
	return &pb.ReleaseResponse{}, nil
}

// SweepOnce releases the reservations that have expired (batches of sweepBatch until none is left) and
// returns how many it released. It does nothing while the service is not ready.
func (s *Service) SweepOnce(ctx context.Context) (int, error) {
	if !s.ready.Load() {
		return 0, nil
	}
	total := 0
	for {
		n, err := s.store.Sweep(ctx, s.clock(), sweepBatch)
		total += n
		if err != nil || n < sweepBatch {
			return total, err
		}
	}
}

// RunSweeper releases expired reservations every SweepEvery until ctx ends. It is per process: with
// several replicas each one sweeps (harmless: the release is conditional on the state under row locks).
func (s *Service) RunSweeper(ctx context.Context) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		t := time.NewTicker(s.sweepEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			sctx, cancel := context.WithTimeout(ctx, sweepTimeout)
			n, err := s.SweepOnce(sctx)
			cancel()
			switch {
			case err != nil && ctx.Err() == nil:
				s.log.WithError(err).Warnf("sweep of expired reservations failed after releasing %d", n)
			case n > 0:
				s.log.Infof("sweep released %d expired reservation(s)", n)
			}
		}
	}()
}
