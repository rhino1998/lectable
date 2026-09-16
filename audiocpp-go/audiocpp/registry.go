package audiocpp

/*
#cgo CFLAGS: -I${SRCDIR}/include
#cgo LDFLAGS: -laudiocpp
#include <stdlib.h>
#include <audiocpp.h>
*/
import "C"

import "unsafe"

// Registry wraps audiocpp_registry (-> engine::runtime::ModelRegistry).
type Registry struct {
	handle *C.audiocpp_registry
}

// NewRegistry creates a registry. configPath may be empty for the default
// loader set (NULL in the C ABI).
func NewRegistry(configPath string) (*Registry, error) {
	cConfigPath, freeConfigPath := optCString(configPath)
	defer freeConfigPath()

	var handle *C.audiocpp_registry
	if err := check(
		C.audiocpp_registry_create(cConfigPath, &handle),
		"audiocpp_registry_create",
	); err != nil {
		return nil, err
	}
	return &Registry{handle: handle}, nil
}

// Close releases the underlying audiocpp_registry. Models loaded from it
// keep working until they are themselves closed (see the package doc's note
// on handle lifetimes). Safe to call more than once, and on a nil *Registry.
func (r *Registry) Close() {
	if r == nil || r.handle == nil {
		return
	}
	C.audiocpp_registry_free(r.handle)
	r.handle = nil
}

// Families lists every model family this registry knows how to load.
func (r *Registry) Families() []string {
	n := int(C.audiocpp_registry_family_count(r.handle))
	out := make([]string, n)
	var family *C.char
	for i := 0; i < n; i++ {
		if err := check(
			C.audiocpp_registry_family(r.handle, C.size_t(i), &family),
			"audiocpp_registry_family",
		); err != nil {
			// The count came from this same registry a moment ago, so an
			// in-range index should not fail; surface a placeholder rather
			// than panicking on what would be an audio.cpp bug.
			out[i] = ""
			continue
		}
		out[i] = C.GoString(family)
	}
	return out
}

// ModelConfig selects which model/config/weights to load, mirroring
// audiocpp_model_config. Every field is optional (empty = unset); FamilyHint
// empty means "identify the model from its path".
type ModelConfig struct {
	// FamilyHint corresponds to --family.
	FamilyHint string
	// ConfigID corresponds to --config, for packages shipping several configs.
	ConfigID string
	// WeightID corresponds to --weight, for packages shipping several weight sets.
	WeightID string
	// ModelSpecOverride corresponds to --model-spec-override.
	ModelSpecOverride string
}

// LoadModel loads a model from modelPath. config and options may be zero
// values / nil.
func (r *Registry) LoadModel(modelPath string, config ModelConfig, options *Options) (*Model, error) {
	cModelPath := C.CString(modelPath)
	defer C.free(unsafe.Pointer(cModelPath))

	cFamilyHint, freeFamilyHint := optCString(config.FamilyHint)
	defer freeFamilyHint()
	cConfigID, freeConfigID := optCString(config.ConfigID)
	defer freeConfigID()
	cWeightID, freeWeightID := optCString(config.WeightID)
	defer freeWeightID()
	cModelSpecOverride, freeModelSpecOverride := optCString(config.ModelSpecOverride)
	defer freeModelSpecOverride()

	cConfig := C.audiocpp_model_config{
		family_hint:         cFamilyHint,
		config_id:           cConfigID,
		weight_id:           cWeightID,
		model_spec_override: cModelSpecOverride,
	}

	var handle *C.audiocpp_model
	if err := check(
		C.audiocpp_model_load(r.handle, cModelPath, &cConfig, options.cOptions(), &handle),
		"audiocpp_model_load",
	); err != nil {
		return nil, err
	}
	return &Model{handle: handle, registry: r}, nil
}
