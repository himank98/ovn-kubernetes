// Code generated by "libovsdb.modelgen"
// DO NOT EDIT.

package sbdb

import "github.com/ovn-org/libovsdb/model"

// SBGlobal defines an object in SB_Global table
type SBGlobal struct {
	UUID        string            `ovsdb:"_uuid"`
	Connections []string          `ovsdb:"connections"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
	Ipsec       bool              `ovsdb:"ipsec"`
	NbCfg       int               `ovsdb:"nb_cfg"`
	Options     map[string]string `ovsdb:"options"`
	SSL         *string           `ovsdb:"ssl"`
}

func copySBGlobalConnections(a []string) []string {
	if a == nil {
		return nil
	}
	b := make([]string, len(a))
	copy(b, a)
	return b
}

func equalSBGlobalConnections(a, b []string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	for i, v := range a {
		if b[i] != v {
			return false
		}
	}
	return true
}

func copySBGlobalExternalIDs(a map[string]string) map[string]string {
	if a == nil {
		return nil
	}
	b := make(map[string]string, len(a))
	for k, v := range a {
		b[k] = v
	}
	return b
}

func equalSBGlobalExternalIDs(a, b map[string]string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}

func copySBGlobalOptions(a map[string]string) map[string]string {
	if a == nil {
		return nil
	}
	b := make(map[string]string, len(a))
	for k, v := range a {
		b[k] = v
	}
	return b
}

func equalSBGlobalOptions(a, b map[string]string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}

func copySBGlobalSSL(a *string) *string {
	if a == nil {
		return nil
	}
	b := *a
	return &b
}

func equalSBGlobalSSL(a, b *string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == b {
		return true
	}
	return *a == *b
}

func (a *SBGlobal) DeepCopyInto(b *SBGlobal) {
	*b = *a
	b.Connections = copySBGlobalConnections(a.Connections)
	b.ExternalIDs = copySBGlobalExternalIDs(a.ExternalIDs)
	b.Options = copySBGlobalOptions(a.Options)
	b.SSL = copySBGlobalSSL(a.SSL)
}

func (a *SBGlobal) DeepCopy() *SBGlobal {
	b := new(SBGlobal)
	a.DeepCopyInto(b)
	return b
}

func (a *SBGlobal) CloneModelInto(b model.Model) {
	c := b.(*SBGlobal)
	a.DeepCopyInto(c)
}

func (a *SBGlobal) CloneModel() model.Model {
	return a.DeepCopy()
}

func (a *SBGlobal) Equals(b *SBGlobal) bool {
	return a.UUID == b.UUID &&
		equalSBGlobalConnections(a.Connections, b.Connections) &&
		equalSBGlobalExternalIDs(a.ExternalIDs, b.ExternalIDs) &&
		a.Ipsec == b.Ipsec &&
		a.NbCfg == b.NbCfg &&
		equalSBGlobalOptions(a.Options, b.Options) &&
		equalSBGlobalSSL(a.SSL, b.SSL)
}

func (a *SBGlobal) EqualsModel(b model.Model) bool {
	c := b.(*SBGlobal)
	return a.Equals(c)
}

var _ model.CloneableModel = &SBGlobal{}
var _ model.ComparableModel = &SBGlobal{}