package meterer

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"slices"
	"time"

	"github.com/Layr-Labs/eigenda/core"
	"github.com/Layr-Labs/eigensdk-go/logging"
	gethcommon "github.com/ethereum/go-ethereum/common"
)

// Config contains network parameters that should be published on-chain. We currently configure these params through disperser env vars.
type Config struct {

	// ChainReadTimeout is the timeout for reading payment state from chain
	ChainReadTimeout time.Duration

	// UpdateInterval is the interval for refreshing the on-chain state
	UpdateInterval time.Duration
}

// Meterer handles payment accounting across different accounts. Disperser API server receives requests from clients and each request contains a blob header
// with payments information (CumulativePayments, Timestamp, and Signature). Disperser will pass the blob header to the meterer, which will check if the
// payments information is valid.
type Meterer struct {
	Config
	// ChainPaymentState reads on-chain payment state periodically and cache it in memory
	ChainPaymentState OnchainPayment
	// OffchainStore uses DynamoDB to track metering and used to validate requests
	OffchainStore OffchainStore

	logger logging.Logger
}

func NewMeterer(
	config Config,
	paymentChainState OnchainPayment,
	offchainStore OffchainStore,
	logger logging.Logger,
) *Meterer {
	return &Meterer{
		Config: config,

		ChainPaymentState: paymentChainState,
		OffchainStore:     offchainStore,

		logger: logger.With("component", "Meterer"),
	}
}

// Start starts to periodically refreshing the on-chain state
func (m *Meterer) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(m.Config.UpdateInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if err := m.ChainPaymentState.RefreshOnchainPaymentState(ctx); err != nil {
					m.logger.Error("Failed to refresh on-chain state", "error", err)
				}
				m.logger.Debug("Refreshed on-chain state")
			case <-ctx.Done():
				return
			}
		}
	}()
}

// MeterRequest validates a blob header and adds it to the meterer's state
// TODO: return error if there's a rejection (with reasoning) or internal error (should be very rare)
func (m *Meterer) MeterRequest(ctx context.Context, header core.PaymentMetadata, numSymbols uint64, quorumNumbers []uint8, receivedAt time.Time) (uint64, error) {
	accountID := gethcommon.HexToAddress(header.AccountID)
	symbolsCharged := m.SymbolsCharged(numSymbols)
	m.logger.Info("Validating incoming request's payment metadata", "paymentMetadata", header, "numSymbols", numSymbols, "quorumNumbers", quorumNumbers)
	// Validate against the payment method
	if header.CumulativePayment.Sign() == 0 {
		reservation, err := m.ChainPaymentState.GetReservedPaymentByAccount(ctx, accountID)
		if err != nil {
			return 0, fmt.Errorf("failed to get active reservation by account: %w", err)
		}
		if err := m.ServeReservationRequest(ctx, header, reservation, symbolsCharged, quorumNumbers, receivedAt); err != nil {
			return 0, fmt.Errorf("invalid reservation: %w", err)
		}
	} else {
		onDemandPayment, err := m.ChainPaymentState.GetOnDemandPaymentByAccount(ctx, accountID)
		if err != nil {
			return 0, fmt.Errorf("failed to get on-demand payment by account: %w", err)
		}
		if err := m.ServeOnDemandRequest(ctx, header, onDemandPayment, symbolsCharged, quorumNumbers, receivedAt); err != nil {
			return 0, fmt.Errorf("invalid on-demand request: %w", err)
		}
	}

	return symbolsCharged, nil
}

// ServeReservationRequest handles the rate limiting logic for incoming requests
func (m *Meterer) ServeReservationRequest(ctx context.Context, header core.PaymentMetadata, reservation *core.ReservedPayment, symbolsCharged uint64, quorumNumbers []uint8, receivedAt time.Time) error {
	m.logger.Info("Recording and validating reservation usage", "header", header, "reservation", reservation)
	if !reservation.IsActiveByNanosecond(header.Timestamp) {
		return fmt.Errorf("reservation not active")
	}
	if err := m.ValidateQuorum(quorumNumbers, reservation.QuorumNumbers); err != nil {
		return fmt.Errorf("invalid quorum for reservation: %w", err)
	}
	reservationWindow := m.ChainPaymentState.GetReservationWindow()
	requestReservationPeriod := GetReservationPeriodByNanosecond(header.Timestamp, reservationWindow)
	if !m.ValidateReservationPeriod(reservation, requestReservationPeriod, receivedAt) {
		return fmt.Errorf("invalid reservation period for reservation")
	}

	// Update bin usage atomically and check against reservation's data rate as the bin limit
	if err := m.IncrementBinUsage(ctx, header, reservation, symbolsCharged, requestReservationPeriod); err != nil {
		return fmt.Errorf("bin overflows: %w", err)
	}

	return nil
}

// ValidateQuorums ensures that the quorums listed in the blobHeader are present within allowedQuorums
// Note: A reservation that does not utilize all of the allowed quorums will be accepted. However, it
// will still charge against all of the allowed quorums. A on-demand requrests require and only allow
// the ETH and EIGEN quorums.
func (m *Meterer) ValidateQuorum(headerQuorums []uint8, allowedQuorums []uint8) error {
	if len(headerQuorums) == 0 {
		return fmt.Errorf("no quorum params in blob header")
	}

	// check that all the quorum ids are in ReservedPayment's
	for _, q := range headerQuorums {
		if !slices.Contains(allowedQuorums, q) {
			// fail the entire request if there's a quorum number mismatch
			return fmt.Errorf("quorum number mismatch: %d", q)
		}
	}
	return nil
}

// ValidateReservationPeriod checks if the provided reservation period is valid
func (m *Meterer) ValidateReservationPeriod(reservation *core.ReservedPayment, requestReservationPeriod uint64, receivedAt time.Time) bool {
	reservationWindow := m.ChainPaymentState.GetReservationWindow()
	currentReservationPeriod := GetReservationPeriod(receivedAt.Unix(), reservationWindow)
	// Valid reservation periodes are either the current bin or the previous bin
	isCurrentOrPreviousPeriod := requestReservationPeriod == currentReservationPeriod || requestReservationPeriod == (currentReservationPeriod-1)
	startPeriod := GetReservationPeriod(int64(reservation.StartTimestamp), reservationWindow)
	endPeriod := GetReservationPeriod(int64(reservation.EndTimestamp), reservationWindow)
	isWithinReservationWindow := startPeriod <= requestReservationPeriod && requestReservationPeriod < endPeriod
	if !isCurrentOrPreviousPeriod || !isWithinReservationWindow {
		return false
	}
	return true
}

// IncrementBinUsage increments the bin usage atomically and checks for overflow
func (m *Meterer) IncrementBinUsage(ctx context.Context, header core.PaymentMetadata, reservation *core.ReservedPayment, symbolsCharged uint64, requestReservationPeriod uint64) error {
	newUsage, err := m.OffchainStore.UpdateReservationBin(ctx, header.AccountID, requestReservationPeriod, symbolsCharged)
	if err != nil {
		return fmt.Errorf("failed to increment bin usage: %w", err)
	}

	// metered usage stays within the bin limit
	usageLimit := m.GetReservationBinLimit(reservation)
	if newUsage <= usageLimit {
		return nil
	} else if newUsage-symbolsCharged >= usageLimit {
		// metered usage before updating the size already exceeded the limit
		return fmt.Errorf("bin has already been filled")
	}
	if newUsage <= 2*usageLimit && requestReservationPeriod+2 <= GetReservationPeriod(int64(reservation.EndTimestamp), m.ChainPaymentState.GetReservationWindow()) {
		_, err := m.OffchainStore.UpdateReservationBin(ctx, header.AccountID, uint64(requestReservationPeriod+2), newUsage-usageLimit)
		if err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("overflow usage exceeds bin limit")
}

// GetReservationPeriodByNanosecondTimestamp returns the current reservation period by chunking nanosecond timestamp by the bin interval;
// bin interval used by the disperser should be public information
func GetReservationPeriodByNanosecond(nanosecondTimestamp int64, binInterval uint64) uint64 {
	if nanosecondTimestamp < 0 {
		return 0
	}
	return GetReservationPeriod(int64((time.Duration(nanosecondTimestamp) * time.Nanosecond).Seconds()), binInterval)
}

// GetReservationPeriod returns the current reservation period by chunking time by the bin interval;
// bin interval used by the disperser should be public information
func GetReservationPeriod(timestamp int64, binInterval uint64) uint64 {
	if binInterval == 0 {
		return 0
	}
	return uint64(timestamp) / binInterval
}

// ServeOnDemandRequest handles the rate limiting logic for incoming requests
// On-demand requests doesn't have additional quorum settings and should only be
// allowed by ETH and EIGEN quorums
func (m *Meterer) ServeOnDemandRequest(ctx context.Context, header core.PaymentMetadata, onDemandPayment *core.OnDemandPayment, symbolsCharged uint64, headerQuorums []uint8, receivedAt time.Time) error {
	m.logger.Info("Recording and validating on-demand usage", "header", header, "onDemandPayment", onDemandPayment)
	quorumNumbers, err := m.ChainPaymentState.GetOnDemandQuorumNumbers(ctx)
	if err != nil {
		return fmt.Errorf("failed to get on-demand quorum numbers: %w", err)
	}

	if err := m.ValidateQuorum(headerQuorums, quorumNumbers); err != nil {
		return fmt.Errorf("invalid quorum for On-Demand Request: %w", err)
	}

	// Validate payments attached
	err = m.ValidatePayment(ctx, header, onDemandPayment, symbolsCharged)
	if err != nil {
		// No tolerance for incorrect payment amounts; no rollbacks
		return fmt.Errorf("invalid on-demand payment: %w", err)
	}

	err = m.OffchainStore.AddOnDemandPayment(ctx, header, symbolsCharged)
	if err != nil {
		return fmt.Errorf("failed to update cumulative payment: %w", err)
	}

	// Update bin usage atomically and check against bin capacity
	if err := m.IncrementGlobalBinUsage(ctx, uint64(symbolsCharged), receivedAt); err != nil {
		//TODO: conditionally remove the payment based on the error type (maybe if the error is store-op related)
		dbErr := m.OffchainStore.RemoveOnDemandPayment(ctx, header.AccountID, header.CumulativePayment)
		if dbErr != nil {
			return dbErr
		}
		return fmt.Errorf("failed global rate limiting: %w", err)
	}

	return nil
}

// ValidatePayment checks if the provided payment header is valid against the local accounting
// prevPmt is the largest  cumulative payment strictly less    than PaymentMetadata.cumulativePayment if exists
// nextPmt is the smallest cumulative payment strictly greater than PaymentMetadata.cumulativePayment if exists
// nextPmtnumSymbols is the numSymbols of corresponding to nextPmt if exists
// prevPmt + PaymentMetadata.numSymbols * m.FixedFeePerByte
// <= PaymentMetadata.CumulativePayment
// <= nextPmt - nextPmtNumSymbols * m.FixedFeePerByte > nextPmt
func (m *Meterer) ValidatePayment(ctx context.Context, header core.PaymentMetadata, onDemandPayment *core.OnDemandPayment, symbolsCharged uint64) error {
	if header.CumulativePayment.Cmp(onDemandPayment.CumulativePayment) > 0 {
		return fmt.Errorf("request claims a cumulative payment greater than the on-chain deposit")
	}

	prevPmt, nextPmt, nextPmtNumSymbols, err := m.OffchainStore.GetRelevantOnDemandRecords(ctx, header.AccountID, header.CumulativePayment) // zero if DNE
	if err != nil {
		return fmt.Errorf("failed to get relevant on-demand records: %w", err)
	}
	// the current request must increment cumulative payment by a magnitude sufficient to cover the blob size
	if new(big.Int).Add(prevPmt, m.PaymentCharged(symbolsCharged)).Cmp(header.CumulativePayment) > 0 {
		return fmt.Errorf("insufficient cumulative payment increment")
	}
	// the current request must not break the payment magnitude for the next payment if the two requests were delivered out-of-order
	if nextPmt.Cmp(big.NewInt(0)) != 0 && new(big.Int).Add(header.CumulativePayment, m.PaymentCharged(uint64(nextPmtNumSymbols))).Cmp(nextPmt) > 0 {
		return fmt.Errorf("breaking cumulative payment invariants")
	}
	// check passed: blob can be safely inserted into the set of payments
	return nil
}

// PaymentCharged returns the chargeable price for a given data length
func (m *Meterer) PaymentCharged(numSymbols uint64) *big.Int {
	// symbolsCharged == m.SymbolsCharged(numSymbols) if numSymbols is already a multiple of MinNumSymbols
	symbolsCharged := big.NewInt(0).SetUint64(m.SymbolsCharged(numSymbols))
	pricePerSymbol := big.NewInt(int64(m.ChainPaymentState.GetPricePerSymbol()))
	return symbolsCharged.Mul(symbolsCharged, pricePerSymbol)
}

// SymbolsCharged returns the number of symbols charged for a given data length
// being at least MinNumSymbols or the nearest rounded-up multiple of MinNumSymbols.
func (m *Meterer) SymbolsCharged(numSymbols uint64) uint64 {
	minSymbols := uint64(m.ChainPaymentState.GetMinNumSymbols())
	if numSymbols <= minSymbols {
		return minSymbols
	}
	// Round up to the nearest multiple of MinNumSymbols
	roundedUp := core.RoundUpDivide(numSymbols, minSymbols) * minSymbols
	// Check for overflow; this case should never happen
	if roundedUp < numSymbols {
		return math.MaxUint64
	}
	return roundedUp
}

// IncrementBinUsage increments the bin usage atomically and checks for overflow
func (m *Meterer) IncrementGlobalBinUsage(ctx context.Context, symbolsCharged uint64, receivedAt time.Time) error {
	globalPeriod := GetReservationPeriod(receivedAt.Unix(), m.ChainPaymentState.GetGlobalRatePeriodInterval())

	newUsage, err := m.OffchainStore.UpdateGlobalBin(ctx, globalPeriod, symbolsCharged)
	if err != nil {
		return fmt.Errorf("failed to increment global bin usage: %w", err)
	}
	if newUsage > m.ChainPaymentState.GetGlobalSymbolsPerSecond()*uint64(m.ChainPaymentState.GetGlobalRatePeriodInterval()) {
		return fmt.Errorf("global bin usage overflows")
	}
	return nil
}

// GetReservationBinLimit returns the bin limit for a given reservation
func (m *Meterer) GetReservationBinLimit(reservation *core.ReservedPayment) uint64 {
	return reservation.SymbolsPerSecond * uint64(m.ChainPaymentState.GetReservationWindow())
}
