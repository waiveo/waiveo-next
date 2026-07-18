// Package model holds the rules/1 authored-rule wire types (contracts/rules-1.md
// "Wire shapes") and a discriminated JSON decoder. Triggers, conditions, and
// actions are a tagged union keyed by "type" (or, for a condition, the
// composition key and/or/not); this package decodes them into a uniform Member
// carrying the discriminant, the optional EntityRef, nested members (choose /
// and/or/not), and the element's remaining raw JSON for later evaluation tasks.
package model

import (
	"encoding/json"
	"fmt"
)

// Rule is the authored top-level object (RUL Wire shapes). Max is a pointer so
// an explicit null (the field's normal absence, RUL-244) is distinguishable
// from a declared value.
type Rule struct {
	ID         string
	Mode       string
	Max        *int
	Triggers   []Member
	Conditions []Member
	Actions    []Member
}

// EntityRef is a trigger's subject or a device-affecting action's target
// (RUL-010): exactly one of the three fields must be set.
type EntityRef struct {
	EntityID    string
	Selector    string
	DeviceClass string
}

// Present counts how many of the three EntityRef fields are non-empty (RUL-010
// / ENTITY_REF_AMBIGUOUS: exactly one is required).
func (e EntityRef) Present() int {
	n := 0
	if e.EntityID != "" {
		n++
	}
	if e.Selector != "" {
		n++
	}
	if e.DeviceClass != "" {
		n++
	}
	return n
}

// Member is one decoded trigger, condition, or action. For a leaf it carries
// Type (the "type" discriminant) and, where the shape has one, EntityRef. For a
// condition composition it carries Composition ("and"/"or"/"not") + Children.
// For a choose action it carries Branches + Default. Raw retains the element's
// full JSON for later evaluation tasks.
type Member struct {
	Type        string     // leaf discriminant ("state", "device_command", ...); "" for a composition
	Composition string     // "and"/"or"/"not" for a condition composition; "" otherwise
	EntityRef   *EntityRef // set when the element carries one
	Children    []Member   // and/or/not nested conditions
	Branches    []Branch   // choose branches
	Default     []Member   // choose default actions
	Raw         json.RawMessage
}

// Branch is one choose branch (RUL Wire shapes): a guard condition + its actions.
type Branch struct {
	Condition Member
	Actions   []Member
}

// ParseRule decodes an authored rule (RUL Wire shapes).
func ParseRule(b []byte) (Rule, error) {
	var top struct {
		ID         string            `json:"id"`
		Mode       string            `json:"mode"`
		Max        *int              `json:"max"`
		Triggers   []json.RawMessage `json:"triggers"`
		Conditions []json.RawMessage `json:"conditions"`
		Actions    []json.RawMessage `json:"actions"`
	}
	if err := json.Unmarshal(b, &top); err != nil {
		return Rule{}, fmt.Errorf("rules/model: parse rule: %w", err)
	}
	r := Rule{ID: top.ID, Mode: top.Mode, Max: top.Max}
	var err error
	if r.Triggers, err = decodeMembers(top.Triggers); err != nil {
		return Rule{}, err
	}
	if r.Conditions, err = decodeMembers(top.Conditions); err != nil {
		return Rule{}, err
	}
	if r.Actions, err = decodeMembers(top.Actions); err != nil {
		return Rule{}, err
	}
	return r, nil
}

func decodeMembers(raws []json.RawMessage) ([]Member, error) {
	out := make([]Member, 0, len(raws))
	for _, raw := range raws {
		m, err := decodeMember(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func decodeMember(raw json.RawMessage) (Member, error) {
	// Peek the shape: composition key, or a "type" discriminant.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Member{}, fmt.Errorf("rules/model: decode member: %w", err)
	}
	m := Member{Raw: raw}

	for _, comp := range []string{"and", "or", "not"} {
		if inner, ok := probe[comp]; ok {
			m.Composition = comp
			var err error
			if comp == "not" {
				child, cerr := decodeMember(inner)
				if cerr != nil {
					return Member{}, cerr
				}
				m.Children = []Member{child}
			} else {
				var arr []json.RawMessage
				if err = json.Unmarshal(inner, &arr); err != nil {
					return Member{}, fmt.Errorf("rules/model: decode %s children: %w", comp, err)
				}
				if m.Children, err = decodeMembers(arr); err != nil {
					return Member{}, err
				}
			}
			return m, nil
		}
	}

	var leaf struct {
		Type        string            `json:"type"`
		EntityID    string            `json:"entity_id"`
		Selector    string            `json:"selector"`
		DeviceClass string            `json:"device_class"`
		Branches    json.RawMessage   `json:"branches"`
		Default     []json.RawMessage `json:"default"`
	}
	if err := json.Unmarshal(raw, &leaf); err != nil {
		return Member{}, fmt.Errorf("rules/model: decode leaf: %w", err)
	}
	m.Type = leaf.Type
	if leaf.EntityID != "" || leaf.Selector != "" || leaf.DeviceClass != "" {
		m.EntityRef = &EntityRef{EntityID: leaf.EntityID, Selector: leaf.Selector, DeviceClass: leaf.DeviceClass}
	}
	if leaf.Type == "choose" {
		var branches []struct {
			Condition json.RawMessage   `json:"condition"`
			Actions   []json.RawMessage `json:"actions"`
		}
		if len(leaf.Branches) > 0 {
			if err := json.Unmarshal(leaf.Branches, &branches); err != nil {
				return Member{}, fmt.Errorf("rules/model: decode choose branches: %w", err)
			}
		}
		for _, b := range branches {
			cond, err := decodeMember(b.Condition)
			if err != nil {
				return Member{}, err
			}
			acts, err := decodeMembers(b.Actions)
			if err != nil {
				return Member{}, err
			}
			m.Branches = append(m.Branches, Branch{Condition: cond, Actions: acts})
		}
		def, err := decodeMembers(leaf.Default)
		if err != nil {
			return Member{}, err
		}
		m.Default = def
	}
	return m, nil
}
