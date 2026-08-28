package symbol

import (
	"github.com/tannevaled/gobig2/internal/arith"
	"github.com/tannevaled/gobig2/internal/page"
)

// Dict is a symbol dictionary.
type Dict struct {
	gbContexts []arith.Ctx
	grContexts []arith.Ctx
	Images     []*page.Image
}

// NewDict creates an empty symbol dictionary.
func NewDict() *Dict {
	return &Dict{}
}

// DeepCopy returns a deep copy of the dictionary.
func (s *Dict) DeepCopy() *Dict {
	dst := NewDict()
	for _, img := range s.Images {
		if img != nil {
			dst.Images = append(dst.Images, img.Duplicate())
		} else {
			dst.Images = append(dst.Images, nil)
		}
	}
	dst.gbContexts = make([]arith.Ctx, len(s.gbContexts))
	copy(dst.gbContexts, s.gbContexts)
	dst.grContexts = make([]arith.Ctx, len(s.grContexts))
	copy(dst.grContexts, s.grContexts)
	return dst
}

// AddImage appends an image to the dictionary.
func (s *Dict) AddImage(image *page.Image) {
	s.Images = append(s.Images, image)
}

// NumImages returns the number of images.
func (s *Dict) NumImages() int {
	return len(s.Images)
}

// GetImage returns the image at index, or nil if out of range.
func (s *Dict) GetImage(index int) *page.Image {
	if index < 0 || index >= len(s.Images) {
		return nil
	}
	return s.Images[index]
}

// GbContexts returns the generic-region arithmetic contexts.
func (s *Dict) GbContexts() []arith.Ctx {
	return s.gbContexts
}

// GrContexts returns the refinement-region arithmetic contexts.
func (s *Dict) GrContexts() []arith.Ctx {
	return s.grContexts
}

// SetGbContexts sets the generic-region arithmetic contexts.
func (s *Dict) SetGbContexts(contexts []arith.Ctx) {
	s.gbContexts = contexts
}

// SetGrContexts sets the refinement-region arithmetic contexts.
func (s *Dict) SetGrContexts(contexts []arith.Ctx) {
	s.grContexts = contexts
}
