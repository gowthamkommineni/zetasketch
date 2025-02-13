package hllplus

import (
	"fmt"
	"math"

	pb "github.com/gowthamkommineni/zetasketch/internal/zetasketch"
)

// Precision bounds.
const (
	MinPrecision       = 10
	MaxPrecision       = 24
	MaxSparsePrecision = 25
)

// HLL is a HyperLogLog++ sketch implementation.
type HLL struct {
	normal []byte
	sparse *sparseState

	precision       uint8
	sparsePrecision uint8
}

// New inits a new sketch.
// The normal precision must be between 10 and 24.
// The sparse precision must be between 0 and 25.
// This function only returns an error when an invalid precision is provided.
func New(precision, sparsePrecision uint8) (*HLL, error) {
	if err := validate(precision, sparsePrecision); err != nil {
		return nil, err
	}

	return &HLL{
		precision:       precision,
		sparsePrecision: sparsePrecision,
		sparse:          newSparseState(precision, sparsePrecision, nil),
	}, nil
}

// NewFromProto inits/restores a sketch from proto message.
func NewFromProto(msg *pb.HyperLogLogPlusUniqueStateProto) (*HLL, error) {
	precision := uint8(msg.GetPrecisionOrNumBuckets())
	sparsePrecision := uint8(msg.GetSparsePrecisionOrNumBuckets())
	if err := validate(precision, sparsePrecision); err != nil {
		return nil, err
	}

	h := &HLL{
		precision:       precision,
		sparsePrecision: sparsePrecision,
	}

	if len(msg.SparseData) > 0 {
		h.sparse = newSparseState(precision, sparsePrecision, msg.SparseData)
	} else {
		h.normal = msg.Data
	}

	return h, nil
}

// Precision returns the normal precision.
func (s *HLL) Precision() uint8 {
	return s.precision
}

// SparsePrecision returns the sparse precision.
func (s *HLL) SparsePrecision() uint8 {
	return s.sparsePrecision
}

// Add adds the uniform hash value to the representation.
func (s *HLL) Add(hash uint64) {
	if s.sparse != nil {
		if s.sparse.Add(hash); s.sparse.OverMax() {
			s.normalize()
		}
		return
	}

	s.ensureNormal()
	pos, rho := computePosRhoW(hash, s.precision)
	if rho > s.normal[pos] {
		s.normal[pos] = rho
	}
}

// Merge merges other into s.
func (s *HLL) Merge(other *HLL) {
	// Skip if there is nothing to merge.
	if len(other.normal) == 0 && other.sparse == nil {
		return
	}

	// FIXME: allow sparse merge
	if s.sparse != nil {
		s.normalize()
	}
	if other.sparse != nil {
		other = other.Clone()
		other.normalize()
	}

	// Make sure receiver is allocated.
	s.ensureNormal()

	// If other precision is higher.
	if s.precision < other.precision {
		other.downgradeEach(s.precision, func(pos uint32, rhoW uint8) {
			if s.normal[pos] < rhoW {
				s.normal[pos] = rhoW
			}
		})
		return
	}

	// If other precision is lower, downgrade.
	if s.precision > other.precision {
		_ = s.Downgrade(other.precision, other.sparsePrecision)
	}

	// Use largest rhoW.
	for i, rho := range other.normal {
		if s.normal[i] < rho {
			s.normal[i] = rho
		}
	}
}

// Clone creates a copy of the sketch.
func (s *HLL) Clone() *HLL {
	clone := &HLL{
		precision:       s.precision,
		sparsePrecision: s.sparsePrecision,
		sparse:          s.sparse.Clone(),
	}
	if len(s.normal) != 0 {
		clone.normal = make([]byte, len(s.normal))
		copy(clone.normal, s.normal)
	}
	return clone
}

// Estimate computes the cardinality estimate according to the algorithm in Figure 6 of the HLL++ paper
// (https://goo.gl/pc916Z).
func (s *HLL) Estimate() int64 {
	if s.sparse != nil {
		s.sparse.Flush()
		return s.sparse.Estimate()
	}

	if len(s.normal) == 0 {
		return 0
	}

	// Compute the summation component of the harmonic mean for the HLL++ algorithm while also
	// keeping track of the number of zeros in case we need to apply LinearCounting instead.
	numZeros := 0
	sum := 0.0

	for _, c := range s.normal {
		if c == 0 {
			numZeros++
		}

		// Compute sum += math.pow(2, -v) without actually performing a floating point exponent
		// computation (which is expensive). v can be at most 64 - precision + 1 and the minimum
		// precision is larger than 2 (see MINIMUM_PRECISION), so this left shift can not overflow.
		x := 1 << c
		sum += 1.0 / float64(x)
	}

	// Return the LinearCount for small cardinalities where, as explained in the HLL++ paper
	// (https://goo.gl/pc916Z), the results with LinearCount tend to be more accurate than with HLL.
	x := 1 << s.precision
	m := float64(x)
	if numZeros != 0 {
		n := int64(m*math.Log(m/float64(numZeros)) + 0.5)
		if n <= linearCountingThreshold(s.precision) {
			return n
		}
	}

	// The "raw" estimate, designated by E in the HLL++ paper (https://goo.gl/pc916Z).
	raw := alpha(s.precision) * m * m / sum

	// Perform bias correction on small estimates. HyperLogLogPlusPlusData only contains bias
	// estimates for small cardinalities and returns 0 for anything else, so the "E < 5m" guard from
	// the HLL++ paper (https://goo.gl/pc916Z) is superfluous here.
	return int64(raw - estimateBias(raw, s.precision) + 0.5)
}

// Downgrade tries to reduce the precision of the sketch.
// Attempts to increase precision will be ignored.
func (s *HLL) Downgrade(precision, sparsePrecision uint8) error {
	if err := validate(precision, sparsePrecision); err != nil {
		return err
	}

	// TODO: downgrade sparse as well (and don't forget a switch between normal and sparse)

	if s.precision > precision {
		if len(s.normal) != 0 {
			normal := make([]byte, 1<<precision)
			s.downgradeEach(precision, func(pos uint32, rhoW uint8) {
				if normal[pos] < rhoW {
					normal[pos] = rhoW
				}
			})
			s.normal = normal
		}
		s.precision = precision
	}

	if s.sparsePrecision > sparsePrecision {
		s.sparsePrecision = sparsePrecision
	}
	return nil
}

func (s *HLL) normalize() {
	if s.sparse == nil {
		return
	}

	s.ensureNormal()
	s.sparse.Iterate(func(pos uint32, rhoW uint8) {
		if rhoW > s.normal[pos] {
			s.normal[pos] = rhoW
		}
	})
	s.sparse = nil
}

func (s *HLL) ensureNormal() {
	if len(s.normal) == 0 {
		s.normal = make([]byte, 1<<s.precision)
	}
}

func (s *HLL) downgradeEach(targetPrecision uint8, iter func(uint32, uint8)) {
	for pos, rho := range s.normal {
		pos2 := pos >> (s.precision - targetPrecision)
		rho2 := normalDowngrade(pos, rho, s.precision, targetPrecision)
		iter(uint32(pos2), rho2)
	}
}

func validate(precision, sparsePrecision uint8) error {
	if precision < MinPrecision || precision > MaxPrecision {
		return fmt.Errorf("invalid normal precision %d", precision)
	}
	if sparsePrecision > MaxSparsePrecision {
		return fmt.Errorf("invalid sparse precision %d", sparsePrecision)
	}
	if sparsePrecision < precision {
		return fmt.Errorf("invalid sparse precision %d: must be >= normal precision %d", sparsePrecision, precision)
	}
	return nil
}

// Proto builds a BigQuery-compatible protobuf message, representing HLL aggregator state.
func (s *HLL) Proto() *pb.HyperLogLogPlusUniqueStateProto {
	// both precisions must always be marshalled:
	precision := int32(s.precision)
	sparsePrecision := int32(s.sparsePrecision)
	msg := &pb.HyperLogLogPlusUniqueStateProto{
		PrecisionOrNumBuckets:       &precision,
		SparsePrecisionOrNumBuckets: &sparsePrecision,
	}

	if s.sparse != nil {
		data, size := s.sparse.GetData()
		size32 := int32(size)
		msg.SparseSize = &size32 // populated to be compatible with zetasketch/BigQuery
		msg.SparseData = data
	} else {
		msg.Data = s.normal
	}
	return msg
}
