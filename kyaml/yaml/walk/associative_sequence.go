// Copyright 2019 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

package walk

import (
	"strings"

	"github.com/go-errors/errors"
	"sigs.k8s.io/kustomize/kyaml/openapi"
	"sigs.k8s.io/kustomize/kyaml/sets"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// appendListNode will append the nodes from src to dst and return dst.
// src and dst should be both sequence node. key is used to call ElementSetter.
// ElementSetter will use key-value pair to find and set the element in sequence
// node.
func appendListNode(dst, src *yaml.RNode, keys []string, merge3 bool) (*yaml.RNode, error) {
	var err error
	for _, elem := range src.Content() {
		// If key is empty, we know this is a scalar value and we can directly set the
		// node
		if keys[0] == "" {
			_, err = dst.Pipe(yaml.ElementSetter{
				Element: elem,
				Keys:    []string{""},
				Values:  []string{elem.Value},
			})
			if err != nil {
				return nil, err
			}
			continue
		}

		if len(keys) > 1 && !merge3 {
			continue
		}

		// we need to get the value for key so that we can find the element to set
		// in sequence.
		v := []string{}
		for _, key := range keys {
			tmpNode := yaml.NewRNode(elem)
			valueNode, err := tmpNode.Pipe(yaml.Get(key))
			if err != nil {
				return nil, err
			}
			if valueNode.IsNil() {
				// no key found, directly append to dst
				err = dst.PipeE(yaml.Append(elem))
				if err != nil {
					return nil, err
				}
				continue
			}
			v = append(v, valueNode.YNode().Value)
		}

		// We use the key and value from elem to find the corresponding element in dst.
		// Then we will use ElementSetter to replace the element with elem. If we cannot
		// find the item, the element will be appended.
		_, err = dst.Pipe(yaml.ElementSetter{
			Element: elem,
			Keys:    keys,
			Values:  v,
		})

		if err != nil {
			return nil, err
		}
	}
	return dst, nil
}

// setPrimitiveSequenceElements sets elements in a primitive list
func (l *Walker) setPrimitiveSequenceElements(values []string, key string, dest *yaml.RNode) (*yaml.RNode, error) {
	// itemsToBeAdded contains the items that will be added to dest
	itemsToBeAdded := yaml.NewListRNode()
	var schema *openapi.ResourceSchema
	if l.Schema != nil {
		schema = l.Schema.Elements()
	}

	for _, value := range values {
		val, err := Walker{
			VisitKeysAsScalars:    l.VisitKeysAsScalars,
			InferAssociativeLists: l.InferAssociativeLists,
			Visitor:               l,
			Schema:                schema,
			Sources:               l.elementValue(key, value),
			MergeOptions:          l.MergeOptions,
		}.Walk()
		if err != nil {
			return nil, err
		}
		// delete the node from **dest** if it's null or empty
		if yaml.IsMissingOrNull(val) || yaml.IsEmptyMap(val) {
			_, err = dest.Pipe(yaml.ElementSetter{Keys: []string{key}, Values: []string{value}})
			if err != nil {
				return nil, err
			}
			continue
		}

		if val.Field(key) == nil {
			// make sure the key is set on the field
			_, err = val.Pipe(yaml.SetField(key, yaml.NewScalarRNode(value)))
			if err != nil {
				return nil, err
			}
		}

		// Add the val to the sequence. val will replace the item in the sequence if
		// there is an item that matches the key-value pair. Otherwise val will be appended
		// the the sequence.
		_, err = itemsToBeAdded.Pipe(yaml.ElementSetter{
			Element: val.YNode(),
			Keys:    []string{key},
			Values:  []string{value},
		})
		if err != nil {
			return nil, err
		}
	}
	var err error
	if l.MergeOptions.ListIncreaseDirection == yaml.MergeOptionsListPrepend {
		// items from patches are needed to be prepended. so we append the
		// dest to itemsToBeAdded
		dest, err = appendListNode(itemsToBeAdded, dest, []string{""}, len(l.Sources) > 2)
	} else {
		// append the items
		dest, err = appendListNode(dest, itemsToBeAdded, []string{""}, len(l.Sources) > 2)
	}
	if err != nil {
		return nil, err
	}
	// sequence is empty
	if yaml.IsMissingOrNull(dest) {
		return nil, nil
	}

	return dest, nil
}

// validateKeys returns a list of valid key-value pairs
// if secondary merge key values are missing, use only the available merge keys
func validateKeys(value []string, keys []string) ([]string, []string) {
	validKeys := make([]string, 0)
	validValues := make([]string, 0)
	for i, v := range value {
		if v != "" {
			validKeys = append(validKeys, keys[i])
			validValues = append(validValues, v)
		}
	}
	if len(validKeys) == 0 { // if values missing, fall back to primary keys
		validKeys = keys
		validValues = value
	}
	return validKeys, validValues
}

// setAssociativeSequenceElements recursively set the elements in the list
func (l *Walker) setAssociativeSequenceElements(values [][]string, keys []string, dest *yaml.RNode) (*yaml.RNode, error) {
	// itemsToBeAdded contains the items that will be added to dest
	itemsToBeAdded := yaml.NewListRNode()
	var schema *openapi.ResourceSchema
	if l.Schema != nil {
		schema = l.Schema.Elements()
	}

	for _, value := range values {
		if len(value) == 0 {
			continue
		}

		validKeys, validValues := validateKeys(value, keys)
		val, err := Walker{
			VisitKeysAsScalars:    l.VisitKeysAsScalars,
			InferAssociativeLists: l.InferAssociativeLists,
			Visitor:               l,
			Schema:                schema,
			Sources:               l.elementValueList(validKeys, validValues),
			MergeOptions:          l.MergeOptions,
		}.Walk()
		if err != nil {
			return nil, err
		}

		exit := false
		for i, key := range validKeys {
			// delete the node from **dest** if it's null or empty
			if yaml.IsMissingOrNull(val) || yaml.IsEmptyMap(val) {
				_, err = dest.Pipe(yaml.ElementSetter{
					Keys:   validKeys,
					Values: validValues,
				})
				if err != nil {
					return nil, err
				}
				exit = true
			} else if val.Field(key) == nil {
				// make sure the key is set on the field
				_, err = val.Pipe(yaml.SetField(key, yaml.NewScalarRNode(validValues[i])))
				if err != nil {
					return nil, err
				}
			}
		}
		if exit {
			continue
		}

		// Add the val to the sequence. val will replace the item in the sequence if
		// there is an item that matches all key-value pairs. Otherwise val will be appended
		// the the sequence.
		_, err = itemsToBeAdded.Pipe(yaml.ElementSetter{
			Element: val.YNode(),
			Keys:    validKeys,
			Values:  validValues,
		})
		if err != nil {
			return nil, err
		}
	}

	var err error
	for _, v := range values {
		validKeys, _ := validateKeys(v, keys)
		if l.MergeOptions.ListIncreaseDirection == yaml.MergeOptionsListPrepend {
			// items from patches are needed to be prepended. so we append the
			// dest to itemsToBeAdded
			dest, err = appendListNode(itemsToBeAdded, dest, validKeys, len(l.Sources) > 2)
		} else {
			// append the items
			dest, err = appendListNode(dest, itemsToBeAdded, validKeys, len(l.Sources) > 2)
		}
	}
	if err != nil {
		return nil, err
	}
	// sequence is empty
	if yaml.IsMissingOrNull(dest) {
		return nil, nil
	}
	return dest, nil
}

func (l *Walker) walkAssociativeSequence() (*yaml.RNode, error) {
	// may require initializing the dest node
	dest, err := l.Sources.setDestNode(l.VisitList(l.Sources, l.Schema, AssociativeList))
	if dest == nil || err != nil {
		return nil, err
	}

	// get the merge key(s) from schema
	var strategy string
	var keys []string
	if l.Schema != nil {
		strategy, keys = l.Schema.PatchStrategyAndKeyList()
	}
	if strategy == "" && len(keys) == 0 { // neither strategy nor keys present in the schema -- infer the key
		// find the list of elements we need to recursively walk
		key, err := l.elementKey()
		if err != nil {
			return nil, err
		}
		if key != "" {
			keys = append(keys, key)
		}
	}

	// non-primitive associative list -- merge the elements
	values := l.elementValues(keys)
	if len(values) != 0 || len(keys) > 0 {
		return l.setAssociativeSequenceElements(values, keys, dest)
	}

	// primitive associative list -- merge the values
	return l.setPrimitiveSequenceElements(l.elementPrimitiveValues(), "", dest)
}

// elementKey returns the merge key to use for the associative list
func (l Walker) elementKey() (string, error) {
	var key string
	for i := range l.Sources {
		if l.Sources[i] != nil && len(l.Sources[i].Content()) > 0 {
			newKey := l.Sources[i].GetAssociativeKey()
			if key != "" && key != newKey {
				return "", errors.Errorf(
					"conflicting merge keys [%s,%s] for field %s",
					key, newKey, strings.Join(l.Path, "."))
			}
			key = newKey
		}
	}
	if key == "" {
		return "", errors.Errorf("no merge key found for field %s",
			strings.Join(l.Path, "."))
	}
	return key, nil
}

// elementValues returns a slice containing all values for the field across all elements
// from all sources.
// Return value slice is ordered using the original ordering from the elements, where
// elements missing from earlier sources appear later.
func (l Walker) elementValues(keys []string) [][]string {
	// use slice to to keep elements in the original order
	var returnValues [][]string
	var seen sets.StringList

	// if we are doing append, dest node should be the first.
	// otherwise dest node should be the last.
	beginIdx := 0
	if l.MergeOptions.ListIncreaseDirection == yaml.MergeOptionsListPrepend {
		beginIdx = 1
	}
	for i := range l.Sources {
		src := l.Sources[(i+beginIdx)%len(l.Sources)]
		if src == nil {
			continue
		}

		// add the value of the field for each element
		// don't check error, we know this is a list node
		values, _ := src.ElementValuesList(keys)
		for _, s := range values {
			if len(s) == 0 || seen.Has(s) {
				continue
			}
			returnValues = append(returnValues, s)
			seen = seen.Insert(s)
		}
	}
	return returnValues
}

// elementPrimitiveValues returns the primitive values in an associative list -- eg. finalizers
func (l Walker) elementPrimitiveValues() []string {
	// use slice to to keep elements in the original order
	var returnValues []string
	seen := sets.String{}
	// if we are doing append, dest node should be the first.
	// otherwise dest node should be the last.
	beginIdx := 0
	if l.MergeOptions.ListIncreaseDirection == yaml.MergeOptionsListPrepend {
		beginIdx = 1
	}
	for i := range l.Sources {
		src := l.Sources[(i+beginIdx)%len(l.Sources)]
		if src == nil {
			continue
		}

		// add the value of the field for each element
		// don't check error, we know this is a list node
		for _, item := range src.YNode().Content {
			if seen.Has(item.Value) {
				continue
			}
			returnValues = append(returnValues, item.Value)
			seen.Insert(item.Value)
		}
	}
	return returnValues
}

// fieldValue returns a slice containing each source's value for fieldName
func (l Walker) elementValue(key, value string) []*yaml.RNode {
	var fields []*yaml.RNode
	for i := range l.Sources {
		if l.Sources[i] == nil {
			fields = append(fields, nil)
			continue
		}
		fields = append(fields, l.Sources[i].Element(key, value))
	}
	return fields
}

// fieldValue returns a slice containing each source's value for fieldName
func (l Walker) elementValueList(keys []string, values []string) []*yaml.RNode {
	var fields []*yaml.RNode
	for i := range l.Sources {
		if l.Sources[i] == nil {
			fields = append(fields, nil)
			continue
		}
		fields = append(fields, l.Sources[i].ElementList(keys, values))
	}
	return fields
}
