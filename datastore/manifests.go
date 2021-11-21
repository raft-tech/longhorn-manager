package datastore

import (
	"bytes"
	"github.com/longhorn/longhorn-manager/types"
	"github.com/pkg/errors"
	"io"
	"io/ioutil"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"path"
)

const (
	ManifestEngineManager = manifest("engine-manager.yaml")
)

type manifest string

// GetPodManifest returns a *v1.Pod based on the specified manifest
//
// An error is returned if the current value of types.SettingNameManifestsPath,
// if the expected manifest file cannot be read, or if an error is returned by
// the decoder.
func (d *DataStore) GetPodManifest(m manifest) (*v1.Pod, error) {
	var src string
	if dir, e := d.GetSetting(types.SettingNameManifestsPath); e == nil {
		src = path.Join(dir.Value, string(m))
	} else {
		return nil, e
	}
	var data io.Reader
	if b, e := ioutil.ReadFile(src); e == nil {
		data = bytes.NewReader(b)
	} else {
		return nil, errors.Wrapf(e, "Unable to read manifest file %s: %v", src, e)
	}
	p := &v1.Pod{}
	err := yaml.NewYAMLToJSONDecoder(data).Decode(p)
	return p, err
}