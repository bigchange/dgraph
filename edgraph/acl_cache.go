// +build !oss

/*
 * Copyright 2018 Dgraph Labs, Inc. All rights reserved.
 *
 * Licensed under the Dgraph Community License (the "License"); you
 * may not use this file except in compliance with the License. You
 * may obtain a copy of the License at
 *
 *     https://github.com/dgraph-io/dgraph/blob/master/licenses/DCL.txt
 */

package edgraph

import (
	"encoding/json"
	"regexp"
	"sync"

	"github.com/dgraph-io/dgraph/ee/acl"
	"github.com/dgraph-io/dgraph/x"
	"github.com/golang/glog"
	"github.com/pkg/errors"
)

type predRegexRule struct {
	predRegex  *regexp.Regexp
	groupPerms map[string]int32
}

// aclCache is the cache mapping group names to the corresponding group acls
type aclCache struct {
	sync.RWMutex
	predPerms      map[string]map[string]int32
	predRegexRules []*predRegexRule
}

var aclCachePtr = &aclCache{
	predPerms:      make(map[string]map[string]int32),
	predRegexRules: make([]*predRegexRule, 0),
}

func (cache *aclCache) update(groups []acl.Group) {
	// In dgraph, acl rules are divided by groups, e.g.
	// the dev group has the following blob representing its ACL rules
	// [friend, 4], [name, 7], [^user.*name$, 4]
	// where friend and name are predicates,
	// and the last one is a regex that can match multiple predicates.
	// However in the aclCachePtr in memory, we need to change the structure so that ACL rules are
	// divided by predicates, e.g.
	// friend ->
	//     dev -> 4
	//     sre -> 6
	// name ->
	//     dev -> 7
	// the reason is that we want to efficiently determine if any ACL rule has been defined
	// for a given predicate, and allow the operation if none is defined, per the fail open
	// approach

	// predPerms is the map descriebed above that maps a single
	// predicate to a submap, and the submap maps a group to a permission
	predPerms := make(map[string]map[string]int32)
	// predRegexPerms is a map from a regex string to a predRegexRule, and a predRegexRule
	// contains a map from a group to a permission
	predRegexPerms := make(map[string]*predRegexRule)
	for _, group := range groups {
		aclBytes := []byte(group.Acls)
		var acls []acl.Acl
		if err := json.Unmarshal(aclBytes, &acls); err != nil {
			glog.Errorf("Unable to unmarshal the aclBytes: %v", err)
			continue
		}

		for _, acl := range acls {
			if len(acl.Predicate) > 0 {
				if groupPerms, found := predPerms[acl.Predicate]; found {
					groupPerms[group.GroupID] = acl.Perm
				} else {
					groupPerms := make(map[string]int32)
					groupPerms[group.GroupID] = acl.Perm
					predPerms[acl.Predicate] = groupPerms
				}
			} else if len(acl.Regex) > 0 {
				if regexRule, found := predRegexPerms[acl.Regex]; found {
					regexRule.groupPerms[group.GroupID] = acl.Perm
				} else {
					predRegex, err := regexp.Compile(acl.Regex)
					if err != nil {
						glog.Errorf("Unable to compile the predicate regex %v "+
							"to create an ACL rule", acl.Regex)
						continue
					}

					groupPermsMap := make(map[string]int32)
					groupPermsMap[group.GroupID] = acl.Perm
					predRegexPerms[acl.Regex] = &predRegexRule{
						predRegex:  predRegex,
						groupPerms: groupPermsMap,
					}
				}
			}
		}
	}

	// convert the predRegexPerms into a slice
	var predRegexRules []*predRegexRule
	for _, predRegexRule := range predRegexPerms {
		predRegexRules = append(predRegexRules, predRegexRule)
	}

	aclCachePtr.Lock()
	defer aclCachePtr.Unlock()
	aclCachePtr.predPerms = predPerms
	aclCachePtr.predRegexRules = predRegexRules
}

func (cache *aclCache) authorizePredicate(groups []string, predicate string,
	operation *acl.Operation) error {
	if x.IsAclPredicate(predicate) {
		return errors.Errorf("only groot is allowed to access the ACL predicate: %s", predicate)
	}

	aclCachePtr.RLock()
	predPerms, predRegexRules := aclCachePtr.predPerms, aclCachePtr.predRegexRules
	aclCachePtr.RUnlock()

	var singlePredMatch bool
	if groupPerms, found := predPerms[predicate]; found {
		singlePredMatch = true
		if hasRequiredAccess(groupPerms, groups, operation) {
			return nil
		}
	}

	var predRegexMatch bool
	for _, predRegexRule := range predRegexRules {
		if predRegexRule.predRegex.MatchString(predicate) {
			predRegexMatch = true
			if hasRequiredAccess(predRegexRule.groupPerms, groups, operation) {
				return nil
			}
		}
	}

	if singlePredMatch || predRegexMatch {
		// there is an ACL rule defined that can match the predicate
		// and the operation has not been allowed
		return errors.Errorf("unauthorized to do %s on predicate %s",
			operation.Name, predicate)
	}

	// no rule has been defined that can match the predicate
	// by default we follow the fail open approach and allow the operation
	return nil
}

// hasRequiredAccess checks if any group in the passed in groups is allowed to perform the operation
// according to the acl rules stored in groupPerms
func hasRequiredAccess(groupPerms map[string]int32, groups []string,
	operation *acl.Operation) bool {
	for _, group := range groups {
		groupPerm, found := groupPerms[group]
		if found && (groupPerm&operation.Code != 0) {
			return true
		}
	}
	return false
}
