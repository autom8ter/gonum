// Copyright ©2016 The gonum Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package distmat

import (
	"math"
	"math/rand"
	"sync"

	"github.com/gonum/mathext"
	"github.com/gonum/matrix"
	"github.com/gonum/matrix/mat64"
	"github.com/gonum/stat/distuv"
)

// Wishart is a distribution over d×d positive symmetric definite matrices. It
// is parametrized by a scalar degrees of freedom parameter ν and a d×d positive
// definite matrix V.
//
// The Wishart PDF is given by
//  p(X) = [|X|^((ν-d-1)/2) * exp(-tr(V^-1 * X)/2)] / [2^(n*d/2) * |V|^(n/2) * Γ(d, ν/2)]
// where X is a d×d PSD matrix, ν > d-1, tr is the trace and Γ is the multivariate gamma function.
//
// See https://en.wikipedia.org/wiki/Wishart_distribution for more information.
type Wishart struct {
	nu  float64
	src *rand.Rand

	dim     int
	cholv   mat64.Cholesky
	logdetv float64
	upper   mat64.TriDense

	once sync.Once
	v    *mat64.SymDense // only stored if needed
}

// NewWishart returns a new Wishart distribution with the given shape matrix and
// degrees of freedom parameter. NewWishart returns whether the creation was
// successful.
//
// NewWishart panics if nu <= p - 1.
func NewWishart(v mat64.Symmetric, nu float64, src *rand.Rand) (*Wishart, bool) {
	dim := v.Symmetric()
	if nu <= float64(dim-1) {
		panic("wishart: nu must be greater than dim-1")
	}
	var chol mat64.Cholesky
	ok := chol.Factorize(v)
	if !ok {
		return nil, false
	}

	var u mat64.TriDense
	u.UFromCholesky(&chol)

	w := &Wishart{
		nu:  nu,
		src: src,

		dim:     dim,
		cholv:   chol,
		logdetv: chol.LogDet(),
		upper:   u,
	}
	return w, true
}

// MeanSym returns the mean matrix of the distribution as a symmetric matrix.
// If x is nil, a new matrix is allocated and returned. If x is not nil, the
// result is stored in-place into x. It must have size d×d or MeanSym will panic.
func (w *Wishart) MeanSym(x *mat64.SymDense) *mat64.SymDense {
	if x == nil {
		x = mat64.NewSymDense(w.dim, nil)
	}
	d := x.Symmetric()
	if d != w.dim {
		panic(badDim)
	}
	w.setV()
	x.CopySym(w.v)
	x.ScaleSym(w.nu, x)
	return x
}

// ProbSym returns the probability of the symmetric matrix x. If x is not positive
// definite (the Cholesky decomposition fails), it has 0 probability.
func (w *Wishart) ProbSym(x mat64.Symmetric) float64 {
	return math.Exp(w.LogProbSym(x))
}

// LogProbSym returns the log of the probability of the input symmetric matrix.
//
// LogProbSym returns -∞ if the input matrix is not positive definite (the Cholesky
// decomposition fails).
func (w *Wishart) LogProbSym(x mat64.Symmetric) float64 {
	dim := x.Symmetric()
	if dim != w.dim {
		panic("dimension mismatch")
	}
	var chol mat64.Cholesky
	ok := chol.Factorize(x)
	if !ok {
		return math.Inf(-1)
	}
	return w.logProbSymChol(&chol)
}

// LogProbSymChol returns the log of the probability of the input symmetric matrix
// given its Cholesky decomposition.
func (w *Wishart) LogProbSymChol(cholX *mat64.Cholesky) float64 {
	dim := cholX.Size()
	if dim != w.dim {
		panic(badDim)
	}
	return w.logProbSymChol(cholX)
}

func (w *Wishart) logProbSymChol(cholX *mat64.Cholesky) float64 {
	// The PDF is
	//  p(X) = [|X|^((ν-d-1)/2) * exp(-tr(V^-1 * X)/2)] / [2^(n*d/2) * |V|^(n/2) * Γ(d, ν/2)]
	// The LogPDF is thus
	// (ν-d-1)/2 * logdet(X) - tr(V^-1 * X)/2  - (ν*d/2)*log(2) - ν/2 * logdet(V) - loggamma(d, ν/2)
	logdetx := cholX.LogDet()

	// Compute tr(V^-1 * X), using the fact that X = U^T * U.
	var u mat64.TriDense
	u.UFromCholesky(cholX)

	var vinvx mat64.Dense
	err := vinvx.SolveCholesky(&w.cholv, u.T())
	if err != nil {
		return math.Inf(-1)
	}
	vinvx.Mul(&vinvx, &u)
	tr := mat64.Trace(&vinvx)

	fnu := float64(w.nu)
	fdim := float64(w.dim)

	return 0.5*((fnu-fdim-1)*logdetx-tr-fnu*fdim*math.Ln2-fnu*w.logdetv) - mathext.MvLgamma(0.5*fnu, w.dim)
}

// RandSym generates a random symmetric matrix from the distribution.
func (w *Wishart) RandSym(x *mat64.SymDense) *mat64.SymDense {
	if x == nil {
		x = &mat64.SymDense{}
	}
	var c mat64.Cholesky
	w.RandChol(&c)
	x.FromCholesky(&c)
	return x
}

// RandChol generates the Cholesky decomposition of a random matrix from the distribution.
func (w *Wishart) RandChol(c *mat64.Cholesky) *mat64.Cholesky {
	// TODO(btracey): Modify the code if the underlying data from c is exposed
	// to avoid the dim^2 allocation here.

	// Use the Bartlett Decomposition, which says that
	//  X ~ L A A^T L^T
	// Where A is a lower triangular matrix in which the diagonal of A is
	// generated from the square roots of χ^2 random variables, and the
	// off-diagonals are generated from standard normal variables.
	// The above gives the cholesky decomposition of X, where L_x = L A.
	//
	// mat64 works with the upper triagular decomposition, so we would like to do
	// the same. We can instead say that
	//  U_x = L_x^T = (L * A)^T = A^T * L^T = A^T * U
	// Instead, generate A^T, by using the procedure above, except as an upper
	// triangular matrix.
	norm := distuv.Normal{
		Mu:     0,
		Sigma:  1,
		Source: w.src,
	}

	t := mat64.NewTriDense(w.dim, matrix.Upper, nil)
	for i := 0; i < w.dim; i++ {
		v := distuv.ChiSquared{
			K:   w.nu - float64(i),
			Src: w.src,
		}.Rand()
		t.SetTri(i, i, math.Sqrt(v))
	}
	for i := 0; i < w.dim; i++ {
		for j := i + 1; j < w.dim; j++ {
			t.SetTri(i, j, norm.Rand())
		}
	}

	t.MulTri(t, &w.upper)
	if c == nil {
		c = &mat64.Cholesky{}
	}
	c.SetFromU(t)
	return c
}

// setV computes and stores the covariance matrix of the distribution.
func (w *Wishart) setV() {
	w.once.Do(func() {
		w.v = mat64.NewSymDense(w.dim, nil)
		w.v.FromCholesky(&w.cholv)
	})
}
