/*
 * Copyright 2016 ClusterHQ
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package fli

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	dlbin "github.com/ClusterHQ/fli/dl/encdec/binary"
	dladler32 "github.com/ClusterHQ/fli/dl/hash/adler32"
	"github.com/ClusterHQ/fli/dp/dataplane"
	"github.com/ClusterHQ/fli/dp/metastore"
	"github.com/ClusterHQ/fli/dp/sync"
	"github.com/ClusterHQ/fli/errors"
	"github.com/ClusterHQ/fli/mdsimpls/restfulstorage"
	"github.com/ClusterHQ/fli/mdsimpls/sqlite3storage"
	"github.com/ClusterHQ/fli/meta/branch"
	"github.com/ClusterHQ/fli/meta/snapshot"
	"github.com/ClusterHQ/fli/meta/volume"
	"github.com/ClusterHQ/fli/meta/volumeset"
	"github.com/ClusterHQ/fli/protocols"
	"github.com/ClusterHQ/fli/securefilepath"
	"github.com/ClusterHQ/fli/version"
	"github.com/ClusterHQ/fli/vh/cauthn"
)

const (
	oneWay = true
	twoWay = false
)

// Handler ...
type Handler struct {
	CfgParams      ConfigParams
	ConfigFile     string
	MdsPathCurrent string
	MdsPathInitial string
	mdsCurrent     metastore.Client
	mdsInitial     metastore.Client
	FliLogFile     string
}

func (c *Handler) upgradeVersion(ver string) error {
	switch ver {
	case "": // Upgrade from older version to 0.7.0
		// TODO It might be a good idea to move upgrade to a different struct?
		// Check if ZPOOL exists
		_, err := exec.Command("zfs", "list", c.CfgParams.Zpool).Output()
		if err != nil {
			log.Printf("ZPOOL %s doesn't exist. Skipping upgrade", c.CfgParams.Zpool)
			return nil
		}

		op, err := exec.Command("zfs", "get", "-H", "-d", "1", "-o", "name", "-t", "filesystem", "name", c.CfgParams.Zpool).Output()
		if err != nil {
			log.Printf("%#v", op)
			return err
		}
		opStr := string(op[:])
		lnResult := strings.Split(opStr, "\n")

		for _, res := range lnResult[1 : len(lnResult)-1] {
			op, err := exec.Command("zfs", "set", "mountpoint=none", res).Output()
			if err != nil {
				log.Printf("%#v", op)
				return err
			}
		}

		// There is no database file available.
		if c.CfgParams.SQLMdsCurrent == "" {
			return nil
		}

		mds, err := c.getMdsCurrent()
		if err != nil {
			return err
		}

		vols, err := metastore.GetAllVolumes(mds)
		if err != nil {
			return err
		}

		for _, vol := range vols {
			path := vol.MntPath.Path()

			// Check if zfs filesystem exists
			_, err := exec.Command("zfs", "list", path[1:]).Output()
			if err != nil {
				log.Printf("ZFS Volume %s doesn't exist. Skipping upgrade", path[1:])
				continue
			}

			// Upgrade the clones mount paths
			op, err := exec.Command("zfs", "set", "mountpoint="+path, path[1:]).Output()
			if err != nil {
				log.Printf("%#v", op)
				return err
			}
		}

		// fallthrough to all future upgrades after this point
	}

	return nil
}

func (c *Handler) upgrade() error {
	if c.CfgParams.Zpool == "" || c.CfgParams.Version == version.Version() {
		return nil
	}

	if err := c.upgradeVersion(c.CfgParams.Version); err != nil {
		return err
	}

	// Update the version in configuration
	c.CfgParams.Version = version.Version()
	cfg := NewConfig(c.ConfigFile)
	if err := cfg.UpdateConfig(c.CfgParams); err != nil {
		return err
	}

	return nil
}

// getMdsCurrent opens the DB connection if it has not been opened before; otherwise returns the existing
// connection.
func (c *Handler) getMdsCurrent() (metastore.Client, error) {
	if c.mdsCurrent != nil {
		return c.mdsCurrent, nil
	}

	mds, err := getMds(c.MdsPathCurrent)
	if err != nil {
		return nil, err
	}

	c.mdsCurrent = mds
	return mds, nil
}

// getMdsInitial opens the DB connection if it has not been opened before; otherwise returns the existing
// connection.
func (c *Handler) getMdsInitial() (metastore.Client, error) {
	if c.mdsInitial != nil {
		return c.mdsInitial, nil
	}

	mds, err := getMds(c.MdsPathInitial)
	if err != nil {
		return nil, err
	}

	c.mdsInitial = mds
	return mds, nil
}

// Clone create a volume from source which could be a snapshot or a branch if more than 1 match found for branch & snapshot together
// should return the matching result found
func (c *Handler) Clone(attributes string, full bool, args []string) (Result, error) {
	cmdOut := CmdOutput{}

	if len(args) < 1 || len(args) > 2 {
		return cmdOut, ErrInvalidArgs{}
	}

	mds, err := c.getMdsCurrent()
	if err != nil {
		return cmdOut, err
	}

	store, err := getStorage(c.CfgParams.Zpool)
	if err != nil {
		return cmdOut, err
	}

	source := args[0]
	volName := ""

	if len(args) == 2 {
		volName = args[1]
	}

	if err := validateName(volName); err != nil {
		return cmdOut, err
	}

	brsFound, err := FindBranches(mds, source)
	if err != nil {
		_, ok := err.(*ErrBranchNotFound)
		if !ok {
			return cmdOut, err
		}
	}

	snapsFound, err := FindSnapshots(mds, source)
	if err != nil {
		_, ok := err.(*ErrSnapshotNotFound)
		if !ok {
			return cmdOut, err
		}
	}

	if len(brsFound) == 0 && len(snapsFound) == 0 {
		return cmdOut, errors.Errorf("Not source (%s) found", source)
	}

	if len(brsFound)+len(snapsFound) > 1 {
		cmdOut.Op = append(cmdOut.Op, CmdResult{Str: "Ambigous matches found for - " + source})

		if len(brsFound) > 0 {
			res := CmdResult{Tab: branchTable(0, full, brsFound)}
			cmdOut.Op = append(cmdOut.Op, res)
		}
		if len(snapsFound) > 0 {
			res := CmdResult{Tab: snapshotTable(0, full, snapsFound)}
			cmdOut.Op = append(cmdOut.Op, res)
		}
	} else {
		var srcSnap *snapshot.Snapshot

		if len(brsFound) == 1 {
			srcSnap = brsFound[0].Tip
		} else {
			srcSnap = snapsFound[0]
		}

		if srcSnap.BlobID.IsNilID() {
			return cmdOut, errors.Errorf("Snapshot %s does not exists local. Pull the snapshot from FlockerHub before using it.", source)
		}

		vol, err := dataplane.CreateVolumeFromSnapshot(mds, store, srcSnap.ID, volName)
		if err != nil {
			return cmdOut, err
		}

		vol.Attrs, err = convStrToAttr(attributes)
		if err != nil {
			return cmdOut, err
		}

		if err := metastore.UpdateVolume(mds, vol); err != nil {
			return cmdOut, err
		}

		cmdOut.Op = append(cmdOut.Op, CmdResult{Str: vol.MntPath.Path()})
	}

	return cmdOut, nil
}

// Snapshot ...
func (c *Handler) Snapshot(branchName string, newBranch bool, attributes string, description string, full bool, args []string) (Result, error) {
	cmdOut := CmdOutput{}

	if branchName != "" && newBranch {
		return cmdOut, errors.New("--branch and --new-branch can not be used together")
	}
	if len(args) < 1 || len(args) > 2 {
		return cmdOut, ErrInvalidArgs{}
	}

	mds, err := c.getMdsCurrent()
	if err != nil {
		return cmdOut, err
	}

	store, err := getStorage(c.CfgParams.Zpool)
	if err != nil {
		return cmdOut, err
	}

	source := args[0]
	snapName := ""

	if len(args) == 2 {
		snapName = args[1]
	}

	if err := validateName(snapName); err != nil {
		return cmdOut, err
	}

	vols, err := FindVolumes(mds, source)
	if err != nil {
		return cmdOut, err
	}

	if len(vols) > 1 {
		cmdOut.Op = append(cmdOut.Op,
			CmdResult{Str: "Ambigous matches found for - " + source,
				Tab: volumeTables(0, full, vols)},
		)
	} else {
		attr, err := convStrToAttr(attributes)
		if err != nil {
			return cmdOut, err
		}

		mode := metastore.AutoSync

		switch {
		case branchName == "" && !newBranch:
			// --branch name is not specified then go to AutoMode
			mode = metastore.AutoSync
		case branchName == "" && newBranch, branchName != "" && !newBranch:
			// 1. --new-branch then create a empty branch in Manual mode
			// 2. --branch name then create a branch with name in Manual mode
			mode = metastore.ManualSync
		default:
			return cmdOut, ErrInvalidArgs{}
		}

		snap, err := dataplane.Snapshot(mds, store, vols[0].ID, branchName, mode, snapName, attr, description)

		if err != nil {
			return cmdOut, err
		}

		cmdOut.Op = append(cmdOut.Op, CmdResult{Str: snap.ID.String()})
	}

	return cmdOut, nil
}

// Create ...
func (c *Handler) Create(attributes string, full bool, args []string) (Result, error) {
	cmdOut := CmdOutput{}

	if len(args) < 1 || len(args) > 2 {
		return cmdOut, ErrInvalidArgs{}
	}

	mds, err := c.getMdsCurrent()
	if err != nil {
		return cmdOut, err
	}

	store, err := getStorage(c.CfgParams.Zpool)
	if err != nil {
		return cmdOut, err
	}

	volsetName := args[0]
	volName := ""

	if len(args) == 2 {
		volName = args[1]
	}

	if err := validateName(volName); err != nil {
		return cmdOut, err
	}

	volsets, err := FindVolumesets(mds, volsetName)
	if err != nil {
		_, ok := err.(*ErrVolSetNotFound)
		if !ok {
			return cmdOut, err
		}

		if err := validateName(volsetName); err != nil {
			return cmdOut, err
		}

		attr, err := convStrToAttr(attributes)
		if err != nil {
			return cmdOut, err
		}

		prefix, vsname := splitVolumeSetName(volsetName)
		vs, err := metastore.VolumeSet(mds, vsname, prefix, attr, "", "", "")
		if err != nil {
			return cmdOut, err
		}

		volsets = append(volsets, vs)
	}

	if len(volsets) > 1 {
		cmdOut.Op = append(cmdOut.Op,
			CmdResult{Str: "Ambigous matches found for - " + volsetName,
				Tab: volumesetTable(0, full, volsets),
			},
		)
	} else {
		vol, err := dataplane.CreateEmptyVolume(mds, store, volsets[0].ID, volName)
		if err != nil {
			return cmdOut, err
		}

		vol.Attrs, err = convStrToAttr(attributes)
		if err != nil {
			return cmdOut, err
		}

		if err := metastore.UpdateVolume(mds, vol); err != nil {
			return cmdOut, err
		}

		cmdOut.Op = append(cmdOut.Op, CmdResult{Str: vol.MntPath.Path()})
	}

	return cmdOut, nil
}

// Init ...
func (c *Handler) Init(attributes string, description string, args []string) (Result, error) {
	cmdOut := CmdOutput{}

	if len(args) > 1 {
		return cmdOut, ErrInvalidArgs{}
	}

	mds, err := c.getMdsCurrent()
	if err != nil {
		return cmdOut, err
	}

	if _, err := getStorage(c.CfgParams.Zpool); err != nil {
		return cmdOut, err
	}

	volsetName := ""
	if len(args) == 1 {
		volsetName = args[0]
	}

	if err := validateName(volsetName); err != nil {
		return cmdOut, err
	}

	attr, err := convStrToAttr(attributes)
	if err != nil {
		return cmdOut, err
	}

	prefix := ""
	vsname := ""

	if volsetName != "" {
		prefix, vsname = splitVolumeSetName(volsetName)
	}

	vs, err := metastore.VolumeSet(mds, vsname, prefix, attr, description, "", "")
	if err != nil {
		return cmdOut, err
	}

	cmdOut.Op = append(cmdOut.Op, CmdResult{Str: vs.ID.String()})

	return cmdOut, nil
}

func (c *Handler) getRestfulMds(fHub, tokenfile string) (*restfulstorage.MetadataStorage, error) {
	if fHub == "" {
		if c.CfgParams.FlockerHubURL == "" {
			return nil, &ErrMissingFlag{FlagName: "url"}
		}

		fHub = c.CfgParams.FlockerHubURL
	}

	if tokenfile == "" {
		if c.CfgParams.AuthTokenFile == "" {
			return nil, &ErrMissingFlag{FlagName: "token"}
		}

		tokenfile = c.CfgParams.AuthTokenFile
	}

	fHubURL, err := url.Parse(fHub)
	if err != nil {
		return nil, err
	}

	fhut := &cauthn.VHUT{}
	err = fhut.InitFromFile(tokenfile)
	if err != nil {
		return nil, err
	}

	return restfulstorage.Create(protocols.GetClient(), fHubURL, fhut)
}

func (c *Handler) sync(url string, token string, all bool, full bool, args []string, syncDirection bool) (Result, error) {
	cmdOut := CmdOutput{}

	if (len(args) != 1 && !all) || (all && len(args) != 0) {
		return cmdOut, ErrInvalidArgs{}
	}

	mdsCurr, err := c.getMdsCurrent()
	if err != nil {
		return cmdOut, err
	}

	if _, err := getStorage(c.CfgParams.Zpool); err != nil {
		return cmdOut, err
	}

	fhMds, err := c.getRestfulMds(url, token)
	if err != nil {
		return cmdOut, err
	}

	volsetName := ""
	if len(args) == 1 {
		volsetName = args[0]
	}

	volsets := []*volumeset.VolumeSet{}

	if all {
		volsets, err = metastore.GetVolumeSets(mdsCurr, volumeset.Query{})
		if err != nil {
			return cmdOut, err
		}

		// at this point there could be more than one volumeset
		if len(volsets) == 0 {
			return cmdOut, errors.New("No volumesets found")
		}
	} else {
		volsetsL, err := FindVolumesets(mdsCurr, volsetName)
		if err != nil {
			_, ok := err.(*ErrVolSetNotFound)
			if !ok {
				return cmdOut, err
			}
		}

		volsetsR, err := FindVolumesets(fhMds, volsetName)
		if err != nil {
			_, ok := err.(*ErrVolSetNotFound)
			if !ok {
				return cmdOut, err
			}
		}

		// Only merge VolSets that are not available locally
		volsets = append(volsets, volsetsL...)
		for _, rVs := range volsetsR {
			found := false
			for _, vs := range volsetsL {
				if rVs.ID.Equals(vs.ID) {
					found = true
					break
				}
			}

			if !found {
				volsets = append(volsets, rVs)
			}
		}

		// At this point, there should only be a single volumeset
		if len(volsets) == 0 {
			return cmdOut, ErrVolSetNotFound{Name: volsetName}
		} else if len(volsets) > 1 {
			cmdOut.Op = append(cmdOut.Op,
				CmdResult{Str: "Ambigous matches found for - " + volsetName,
					Tab: volumesetTable(0, full, volsets),
				},
			)
		}
	}

	for _, volset := range volsets {
		mdsInit, err := c.getMdsInitial()
		if err != nil {
			return cmdOut, err
		}

		conflicts, err := sync.Do(fhMds, mdsCurr, mdsInit, volset.ID, syncDirection)
		if err != nil {
			return cmdOut, err
		}

		if conflicts.HasConflicts() {
			cmdOut.Op = append(cmdOut.Op, CmdResult{Str: fmt.Sprintf("Volumeset  %v has conflicts", volset.ID.String())})
			for _, v := range conflicts.VsC {
				res := CmdResult{}
				res.Str = "VolumeSet conflict:"
				res.Tab = append(res.Tab, []string{"Initial version:", fmt.Sprintf("%v", v.Init)})
				res.Tab = append(res.Tab, []string{"Current version (overwritten by target one):", fmt.Sprintf("%v", v.Cur)})
				res.Tab = append(res.Tab, []string{"Target version:", fmt.Sprintf("%v", v.Tgt)})

				cmdOut.Op = append(cmdOut.Op, res)
			}

			for _, s := range conflicts.SnC {
				res := CmdResult{}
				res.Str = "Snapshot conflict:"
				res.Tab = append(res.Tab, []string{"Initial version:", fmt.Sprintf("%v", s.Init)})
				res.Tab = append(res.Tab, []string{"Current version (overwritten by target one):", fmt.Sprintf("%v", s.Cur)})
				res.Tab = append(res.Tab, []string{"Target version:", fmt.Sprintf("%v", s.Tgt)})

				cmdOut.Op = append(cmdOut.Op, res)
			}

			for _, b := range conflicts.BrC {
				res := CmdResult{}
				res.Str = "Branch conflict:"
				res.Tab = append(res.Tab, []string{"Initial version:", fmt.Sprintf("%v", b.Init)})
				res.Tab = append(res.Tab, []string{"Current version (overwritten by target one):", fmt.Sprintf("%v", b.Cur)})
				res.Tab = append(res.Tab, []string{"Target version:", fmt.Sprintf("%v", b.Tgt)})

				cmdOut.Op = append(cmdOut.Op, res)
			}
		}
	}

	return cmdOut, nil
}

// Sync ...
func (c *Handler) Sync(url string, token string, all bool, full bool, args []string) (Result, error) {
	return c.sync(url, token, all, full, args, twoWay)
}

// Fetch ...
func (c *Handler) Fetch(url string, token string, all bool, full bool, args []string) (Result, error) {
	return c.sync(url, token, all, full, args, oneWay)
}

// Push ...
func (c *Handler) Push(url string, token string, full bool, args []string) (Result, error) {
	cmdOut := CmdOutput{}

	if len(args) != 1 {
		return cmdOut, ErrInvalidArgs{}
	}

	mds, err := c.getMdsCurrent()
	if err != nil {
		return cmdOut, err
	}

	store, err := getStorage(c.CfgParams.Zpool)
	if err != nil {
		return cmdOut, err
	}

	name := args[0]

	volsets, err := FindVolumesets(mds, name)
	if err != nil {
		_, ok := err.(*ErrVolSetNotFound)
		if !ok {
			return cmdOut, err
		}
	}

	snaps, err := FindSnapshots(mds, name)
	if err != nil {
		_, isNotFound := err.(*ErrSnapshotNotFound)
		_, isInvalidSearch := err.(*ErrInvalidSearch)

		if !isNotFound && !isInvalidSearch {
			return cmdOut, err
		}
	}

	if len(volsets) == 0 && len(snaps) == 0 {
		return cmdOut, errors.Errorf("Volumeset or Snapshot (%s) not found", name)
	}

	if len(volsets)+len(snaps) > 1 {
		cmdOut.Op = append(cmdOut.Op, CmdResult{Str: "Ambigous matches found for - " + name})

		if len(volsets) > 0 {
			res := CmdResult{Tab: volumesetTable(0, full, volsets)}
			cmdOut.Op = append(cmdOut.Op, res)
		}
		if len(snaps) > 0 {
			res := CmdResult{Tab: snapshotTable(0, full, snaps)}
			cmdOut.Op = append(cmdOut.Op, res)
		}

		return cmdOut, nil
	}

	fhMds, err := c.getRestfulMds(url, token)
	if err != nil {
		return cmdOut, err
	}

	// TODO: Make record encoder/decoder configurable
	encdec := dlbin.Factory{}
	hf := dladler32.Factory{}
	if len(snaps) == 1 {
		if err = sync.PushDataForCertainSnapshots(mds, &blobDiff{store: store, ed: encdec, hf: hf}, fhMds,
			[]snapshot.ID{snaps[0].ID}); err != nil {
			return cmdOut, err
		}
	} else {
		if err = sync.PushDataForAllSnapshots(mds, volsets[0].ID, &blobDiff{store: store, ed: encdec, hf: hf},
			fhMds); err != nil {
			return cmdOut, err
		}
	}

	return cmdOut, nil
}

// Pull ...
func (c *Handler) Pull(url string, token string, full bool, args []string) (Result, error) {
	cmdOut := CmdOutput{}

	if len(args) != 1 {
		return cmdOut, ErrInvalidArgs{}
	}

	mds, err := c.getMdsCurrent()
	if err != nil {
		return cmdOut, err
	}

	store, err := getStorage(c.CfgParams.Zpool)
	if err != nil {
		return cmdOut, err
	}

	name := args[0]

	volsets, err := FindVolumesets(mds, name)
	if err != nil {
		_, ok := err.(*ErrVolSetNotFound)
		if !ok {
			return cmdOut, err
		}
	}

	snaps, err := FindSnapshots(mds, name)
	if err != nil {
		_, isNotFound := err.(*ErrSnapshotNotFound)
		_, isInvalidSearch := err.(*ErrInvalidSearch)

		if !isNotFound && !isInvalidSearch {
			return cmdOut, err
		}
	}

	if len(volsets) == 0 && len(snaps) == 0 {
		return cmdOut, errors.Errorf("Volumeset or Snapshot (%s) not found", name)
	}

	if len(volsets)+len(snaps) > 1 {
		cmdOut.Op = append(cmdOut.Op, CmdResult{Str: "Ambigous matches found for - " + name})

		if len(volsets) > 0 {
			res := CmdResult{Tab: volumesetTable(0, full, volsets)}
			cmdOut.Op = append(cmdOut.Op, res)
		}
		if len(snaps) > 0 {
			res := CmdResult{Tab: snapshotTable(0, full, snaps)}
			cmdOut.Op = append(cmdOut.Op, res)
		}

		return cmdOut, nil
	}

	fhMds, err := c.getRestfulMds(url, token)
	if err != nil {
		return cmdOut, err
	}

	// TODO: Make record encoder/decoder configurable
	encdec := dlbin.Factory{}
	hf := dladler32.Factory{}
	if len(snaps) == 1 {
		if err = sync.PullDataForCertainSnapshots(fhMds, mds, &blobDiff{store: store, ed: encdec, hf: hf},
			[]snapshot.ID{snaps[0].ID}); err != nil {
			return cmdOut, err
		}
	} else {
		if err = sync.PullDataForAllSnapshots(fhMds, mds, volsets[0].ID,
			&blobDiff{store: store, ed: encdec, hf: hf}); err != nil {
			return cmdOut, err
		}
	}

	return cmdOut, nil
}

// Update ...
func (c *Handler) Update(name string, attributes string, description string, full bool, args []string) (Result, error) {
	cmdOut := CmdOutput{}

	if len(args) != 1 {
		return cmdOut, ErrInvalidArgs{}
	}

	mds, err := c.getMdsCurrent()
	if err != nil {
		return cmdOut, err
	}

	if _, err := getStorage(c.CfgParams.Zpool); err != nil {
		return cmdOut, err
	}

	source := args[0]

	snapFound, brFound, volFound, err := FindAll(mds, source)
	if err != nil {
		return cmdOut, err
	}

	volsetFound, err := FindVolumesets(mds, source)
	if err != nil {
		_, ok := err.(*ErrVolSetNotFound)
		if !ok {
			return cmdOut, err
		}
	}

	switch {
	case len(brFound)+len(snapFound)+len(volFound)+len(volsetFound) == 0:
		return cmdOut, errors.Errorf("No matching objects found for '%s'", source)

	case len(brFound)+len(snapFound)+len(volFound)+len(volsetFound) > 1:
		cmdOut.Op = append(cmdOut.Op, CmdResult{Str: "Ambigous matches found for - " + source})

		if len(volsetFound) > 0 {
			res := CmdResult{Tab: volumesetTable(0, full, volsetFound)}
			cmdOut.Op = append(cmdOut.Op, res)
		}

		if len(snapFound) > 0 {
			res := CmdResult{Tab: snapshotTable(0, full, snapFound)}
			cmdOut.Op = append(cmdOut.Op, res)
		}

		if len(brFound) > 0 {
			res := CmdResult{Tab: branchTable(0, full, brFound)}
			cmdOut.Op = append(cmdOut.Op, res)
		}

		if len(volFound) > 0 {
			res := CmdResult{Tab: volumeTables(0, full, volFound)}
			cmdOut.Op = append(cmdOut.Op, res)
		}

	default:
		switch {
		case len(volsetFound) == 1:
			if name != "" {
				if err := validateName(name); err != nil {
					return cmdOut, err
				}

				volsetFound[0].Name = name
			}
			if attributes != "" {
				nAttrs, err := convStrToAttr(attributes)
				if err != nil {
					return cmdOut, err
				}

				fAttrs := updateAttributes(nAttrs, volsetFound[0].Attrs)
				volsetFound[0].Attrs = fAttrs
			}
			if description != "" {
				volsetFound[0].Description = description
			}

			if err := metastore.UpdateVolumeSet(mds, volsetFound[0]); err != nil {
				return cmdOut, err
			}

		case len(snapFound) == 1:
			if name != "" {
				if err := validateName(name); err != nil {
					return cmdOut, err
				}

				snapFound[0].Name = name
			}
			if attributes != "" {
				nAttrs, err := convStrToAttr(attributes)
				if err != nil {
					return cmdOut, err
				}

				fAttrs := updateAttributes(nAttrs, snapFound[0].Attrs)
				snapFound[0].Attrs = fAttrs
			}
			if description != "" {
				snapFound[0].Description = description
			}

			if err := metastore.UpdateSnapshot(mds, snapFound[0]); err != nil {
				return cmdOut, err
			}

		case len(brFound) == 1:
			if name == "" {
				return cmdOut, ErrMissingFlag{FlagName: "name"}
			}
			if attributes != "" || description != "" {
				return cmdOut, ErrInvalidArgs{}
			}

			if err := validateName(name); err != nil {
				return cmdOut, err
			}

			if err := metastore.RenameBranch(mds, brFound[0].Tip.VolSetID, brFound[0].Name, name); err != nil {
				return cmdOut, err
			}

		default:
			if name != "" {
				if err := validateName(name); err != nil {
					return cmdOut, err
				}

				volFound[0].Name = name
			}
			if attributes != "" {
				nAttrs, err := convStrToAttr(attributes)
				if err != nil {
					return cmdOut, err
				}

				fAttrs := updateAttributes(nAttrs, volFound[0].Attrs)
				volFound[0].Attrs = fAttrs
			}
			if description != "" {
				return cmdOut, ErrInvalidArgs{}
			}

			if err := metastore.UpdateVolume(mds, volFound[0]); err != nil {
				return cmdOut, err
			}
		}
	}

	return cmdOut, nil
}

// Remove ...
func (c *Handler) Remove(full bool, args []string) (Result, error) {
	cmdOut := CmdOutput{}

	if len(args) != 1 {
		return cmdOut, ErrInvalidArgs{}
	}

	mds, err := c.getMdsCurrent()
	if err != nil {
		return cmdOut, err
	}

	store, err := getStorage(c.CfgParams.Zpool)
	if err != nil {
		return cmdOut, err
	}

	source := args[0]

	snapFound, brFound, volFound, err := FindAll(mds, source)
	if err != nil {
		return cmdOut, err
	}

	volsetFound, err := FindVolumesets(mds, source)
	if err != nil {
		_, ok := err.(*ErrVolSetNotFound)
		if !ok {
			return cmdOut, err
		}
	}

	switch {
	case len(brFound)+len(snapFound)+len(volFound)+len(volsetFound) == 0:
		return cmdOut, errors.Errorf("No matching objects found for '%s'", source)

	case len(brFound)+len(snapFound)+len(volFound)+len(volsetFound) > 1:
		cmdOut.Op = append(cmdOut.Op, CmdResult{Str: "Ambigous matches found for - " + source})

		if len(volsetFound) > 0 {
			res := CmdResult{Tab: volumesetTable(0, full, volsetFound)}
			cmdOut.Op = append(cmdOut.Op, res)
		}
		if len(snapFound) > 0 {
			res := CmdResult{Tab: snapshotTable(0, full, snapFound)}
			cmdOut.Op = append(cmdOut.Op, res)
		}
		if len(brFound) > 0 {
			res := CmdResult{Tab: branchTable(0, full, brFound)}
			cmdOut.Op = append(cmdOut.Op, res)
		}
		if len(volFound) > 0 {
			res := CmdResult{Tab: volumeTables(0, full, volFound)}
			cmdOut.Op = append(cmdOut.Op, res)
		}

	default:
		switch {
		case len(volsetFound) == 1:
			if err := dataplane.DeleteVolumeSet(mds, store, volsetFound[0].ID); err != nil {
				return cmdOut, err
			}

		case len(snapFound) == 1:
			if err := dataplane.DeleteBlob(mds, store, snapFound[0].ID); err != nil {
				return cmdOut, err
			}

		case len(brFound) == 1:
			if err := dataplane.DeleteBranch(mds, store, brFound[0].Tip.VolSetID, brFound[0]); err != nil {
				return cmdOut, err
			}

		default:
			if err := dataplane.DeleteVolume(mds, store, volFound[0].ID); err != nil {
				return cmdOut, err
			}
		}
	}

	return cmdOut, nil
}

// List ...
func (c *Handler) List(
	all bool,
	volumeFlag bool,
	snapshotFlag bool,
	branchFlag bool,
	full bool,
	args []string,
) (Result, error) {
	cmdOut := CmdOutput{}

	if len(args) > 1 {
		return cmdOut, ErrInvalidArgs{}
	}

	search := ""
	if len(args) == 1 {
		search = args[0]
	}

	mds, err := c.getMdsCurrent()
	if err != nil {
		return cmdOut, err
	}

	if _, err := getStorage(c.CfgParams.Zpool); err != nil {
		return cmdOut, err
	}

	vsFound, err := FindVolumesets(mds, search)
	if err != nil {
		if _, ok := err.(*ErrVolSetNotFound); !ok {
			return cmdOut, err
		}
	}

	// report only the volumesets
	if !all && !volumeFlag && !snapshotFlag && !branchFlag && search == "" {
		return ListResult{
			full: full,
			vols: vsFound,
		}, nil
	}

	// display all objects of vs
	// if --all or
	// if no flags then search every object
	if all || (!all && !volumeFlag && !snapshotFlag && !branchFlag) {
		volumeFlag, snapshotFlag, branchFlag = true, true, true
	}

	vsObjMap := make(map[volumeset.ID]volumesetObjects)
	for _, vs := range vsFound {
		vsObjMap[vs.ID] = volumesetObjects{volset: vs}
	}

	if all {
		for vsID, vsObj := range vsObjMap {
			snapFound, brFound, volFound, err := FindAll(mds, vsID.String()+":")
			if err != nil {
				return cmdOut, err
			}

			vsObj.snaps = snapFound
			vsObj.brs = brFound
			vsObj.vols = volFound

			vsObjMap[vsID] = vsObj
		}

		if search != "" {
			// if a search string is provided then match it with objects other than volumeset
			tmpVSObjMap := make(map[volumeset.ID]volumesetObjects)

			snapFound, err := FindSnapshots(mds, search)
			if err != nil {
				_, isNotFound := err.(*ErrSnapshotNotFound)
				if !isNotFound {
					return cmdOut, err
				}
			}
			for _, snap := range snapFound {
				if _, ok := vsObjMap[snap.VolSetID]; !ok {
					vs, _ := FindVolumesets(mds, snap.VolSetID.String())
					tmpVSObjMap[snap.VolSetID] = volumesetObjects{volset: vs[0], snaps: []*snapshot.Snapshot{snap}}
				}
			}

			brFound, err := FindBranches(mds, search)
			if err != nil {
				_, isNotFound := err.(*ErrBranchNotFound)
				if !isNotFound {
					return cmdOut, err
				}
			}
			for _, br := range brFound {
				if _, ok := vsObjMap[br.Tip.VolSetID]; !ok {
					vs, _ := FindVolumesets(mds, br.Tip.VolSetID.String())
					tmpVSObjMap[br.Tip.VolSetID] = volumesetObjects{volset: vs[0], brs: []*branch.Branch{br}}
				}
			}

			volFound, err := FindVolumes(mds, search)
			if err != nil {
				_, isNotFound := err.(*ErrVolumeNotFound)
				if !isNotFound {
					return cmdOut, err
				}
			}
			for _, vol := range volFound {
				if _, ok := vsObjMap[vol.VolSetID]; !ok {
					vs, _ := FindVolumesets(mds, vol.VolSetID.String())
					vsObjMap[vol.VolSetID] = volumesetObjects{volset: vs[0], vols: []*volume.Volume{vol}}
				}
			}

			// Add the missing volumeset founds by finding matching objects from VolumeSets
			for vsID, vsObj := range tmpVSObjMap {
				vsObjMap[vsID] = vsObj
			}
		}
	} else {
		if snapshotFlag {
			snapFound, err := FindSnapshots(mds, search)
			if err != nil {
				_, isNotFound := err.(*ErrSnapshotNotFound)
				if !isNotFound {
					return cmdOut, err
				}
			}

			for _, snap := range snapFound {
				snapSlice := []*snapshot.Snapshot{snap}

				if val, ok := vsObjMap[snap.VolSetID]; ok {
					// key found
					val.snaps = append(val.snaps, snapSlice...)
					vsObjMap[snap.VolSetID] = val
				} else {
					vs, _ := FindVolumesets(mds, snap.VolSetID.String())
					vsObjMap[snap.VolSetID] = volumesetObjects{volset: vs[0], snaps: snapSlice}
				}
			}
		}

		if branchFlag {
			brFound, err := FindBranches(mds, search)
			if err != nil {
				_, isNotFound := err.(*ErrBranchNotFound)
				if !isNotFound {
					return cmdOut, err
				}
			}

			for _, br := range brFound {
				brSlice := []*branch.Branch{br}

				if val, ok := vsObjMap[br.Tip.VolSetID]; ok {
					// key found
					val.brs = append(val.brs, brSlice...)
					vsObjMap[br.Tip.VolSetID] = val
				} else {
					vs, _ := FindVolumesets(mds, br.Tip.VolSetID.String())
					vsObjMap[br.Tip.VolSetID] = volumesetObjects{volset: vs[0], brs: brSlice}
				}
			}
		}

		if volumeFlag {
			volFound, err := FindVolumes(mds, search)
			if err != nil {
				_, isNotFound := err.(*ErrVolumeNotFound)
				if !isNotFound {
					return cmdOut, err
				}
			}

			for _, vol := range volFound {
				volSlice := []*volume.Volume{vol}

				if val, ok := vsObjMap[vol.VolSetID]; ok {
					// key found
					val.vols = append(val.vols, volSlice...)
					vsObjMap[vol.VolSetID] = val
				} else {
					vs, _ := FindVolumesets(mds, vol.VolSetID.String())
					vsObjMap[vol.VolSetID] = volumesetObjects{volset: vs[0], vols: volSlice}
				}
			}
		}
	}

	result := ListResult{full: full}
	for _, vsObj := range vsObjMap {
		obj := vsObj
		result.vsObjs = append(result.vsObjs, &obj)
	}

	return result, nil
}

// Setup is called when fli is setting up the system
func (c *Handler) Setup(zpool string, force bool, args []string) (Result, error) {
	if len(args) > 0 {
		return CmdOutput{}, ErrInvalidArgs{}
	}

	mdsCurrentFPath, err := securefilepath.New(c.MdsPathCurrent)
	if err != nil {
		return CmdOutput{}, err
	}

	mdsInitialFPath, err := securefilepath.New(c.MdsPathInitial)
	if err != nil {
		return CmdOutput{}, err
	}

	currentExists, err := mdsCurrentFPath.Exists()
	if err != nil {
		return CmdOutput{}, err
	}

	initialExists, err := mdsInitialFPath.Exists()
	if err != nil {
		return CmdOutput{}, err
	}

	if currentExists || initialExists {
		// To be able to recreate the files you need force flag to be passed
		if !force {
			// You are trying to re-create the files without using forces
			return CmdOutput{}, errors.Errorf("Metadata store file already exists. Use --force to reset the store.")
		}

		os.RemoveAll(mdsCurrentFPath.Path())
		os.RemoveAll(mdsInitialFPath.Path())
	}

	if zpool == "" {
		// Without ZPOOL fli can't proceed
		return CmdOutput{}, errors.Errorf("zpool not set for the fli client. Use --zpool to set the zpool")
	}

	c.CfgParams.Version = version.Version()
	c.CfgParams.SQLMdsInitial = mdsInitialFPath.Path()
	c.CfgParams.SQLMdsCurrent = mdsCurrentFPath.Path()
	c.CfgParams.Zpool = zpool

	if _, err := getStorage(c.CfgParams.Zpool); err != nil {
		return CmdOutput{}, err
	}

	if _, err := sqlite3storage.Create(mdsCurrentFPath); err != nil {
		return CmdOutput{}, err
	}

	if _, err := sqlite3storage.Create(mdsInitialFPath); err != nil {
		return CmdOutput{}, err
	}

	cfg := NewConfig(c.ConfigFile)
	if err := cfg.UpdateConfig(c.CfgParams); err != nil {
		return CmdOutput{}, err
	}

	return CmdOutput{}, nil
}

// Config ...
func (c *Handler) Config(url string, token string, offline bool, args []string) (Result, error) {
	cmdOut := CmdOutput{}

	if len(args) > 0 {
		return cmdOut, ErrInvalidArgs{}
	}

	if url != "" {
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			url = "https://" + url
		}

		c.CfgParams.FlockerHubURL = url
	}

	if token != "" {
		if !filepath.IsAbs(token) {
			return cmdOut, errors.Errorf("Token file (%s) is not an absolute path", token)
		}

		if _, err := os.Stat(token); os.IsNotExist(err) {
			return cmdOut, errors.Errorf("Token file (%s) does not exist", token)
		}

		c.CfgParams.AuthTokenFile = token
	}

	cfg := NewConfig(c.ConfigFile)
	if err := cfg.UpdateConfig(c.CfgParams); err != nil {
		return CmdOutput{}, err
	}

	if !offline {
		if c.CfgParams.FlockerHubURL == "" {
			return cmdOut, errors.New(`FlockerHub URL is not configured.
To skip URL validation use --offline option`)
		}

		if c.CfgParams.AuthTokenFile == "" {
			return cmdOut, errors.New(`FLockerHub URL validation failed, authentication token file is not configured.
To skip URL validation use --offline option`)
		}

		// Analytics notification that validates URL
		analyticsLogger := NewAnalyticsLogger(c.CfgParams.FlockerHubURL, c.CfgParams.AuthTokenFile)
		if err := analyticsLogger.LogConfig(); err != nil {
			return cmdOut, err
		}
	}

	return cmdOut, nil
}

// Version ...
func (c *Handler) Version(args []string) (Result, error) {
	tab := [][]string{}

	tab = append(tab, []string{"Version:", version.Version()})
	commitID := version.CommitID()
	if commitID != "" && len(commitID) == 40 {
		// git hash should be 40 char long
		tab = append(tab, []string{"Git commit:", commitID[:7]})
	}

	return CmdOutput{Op: []CmdResult{{Tab: tab}}}, nil
}

func addFileToTarball(tw *tar.Writer, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return err
	}

	// now lets create the header as needed for this file within the tarball
	header, err := tar.FileInfoHeader(stat, "") //no links
	if err != nil {
		return err
	}

	// write the header to the tarball archive
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	// copy the file data to the tarball
	if _, err := io.Copy(tw, file); err != nil {
		return err
	}

	return nil
}

func dumpZfsStats(pool, filepath string) error {
	output, err := exec.Command("zfs", "list", "-rt", "all", pool).Output()
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(filepath, output, 0644)
	if err != nil {
		return err
	}
	return nil
}

func dumpZpoolHistory(pool, filepath string) error {
	output, err := exec.Command("zpool", "history", "-li", pool).Output()
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(filepath, output, 0644)
	if err != nil {
		return err
	}
	return nil
}

// Diagnostics ...
func (c *Handler) Diagnostics(args []string) (Result, error) {
	tab := [][]string{}

	if len(args) != 1 {
		return CmdOutput{}, ErrInvalidArgs{}
	}
	dir := args[0]

	strTime := time.Now().String()
	// spaces and colons are bad (special chars wrt gzip).
	// replace them with underscores and dashes respectively.
	strTime = strings.Replace(strTime, " ", "_", -1)
	strTime = strings.Replace(strTime, ":", "-", -1)

	archiveName := "chq_diag" + "_" + strTime + ".tar" + ".gz"
	zfsDumpName := "chq_zfs_stats" + "_" + strTime
	zpoolHistName := "chq_zpool_history" + "_" + strTime
	fliInfoName := "chq_fli_info" + "_" + strTime

	archivePath := filepath.Join(dir, archiveName)
	zfsDumpPath := filepath.Join(dir, zfsDumpName)
	zpoolDumpPath := filepath.Join(dir, zpoolHistName)
	fliInfoPath := filepath.Join(dir, fliInfoName)

	// create archive
	archive, err := os.Create(archivePath)
	if err != nil {
		return CmdOutput{}, err
	}
	defer func() {
		if err != nil {
			os.Remove(archivePath)
		} else {
			archive.Close()
		}
	}()

	// dump zfs stats to a temp file
	if err := dumpZfsStats(c.CfgParams.Zpool, zfsDumpPath); err != nil {
		os.Remove(zfsDumpPath) //paranoid
		return CmdOutput{}, err
	}
	defer os.Remove(zfsDumpPath)

	// dump zpool history to a temp file
	if err := dumpZpoolHistory(c.CfgParams.Zpool, zpoolDumpPath); err != nil {
		os.Remove(zpoolDumpPath) //paranoid
		return CmdOutput{}, err
	}
	defer os.Remove(zpoolDumpPath)

	// dump version info to a temp file
	info, err := c.Info([]string{})
	if err != nil {
		return CmdOutput{}, err
	}
	if err = ioutil.WriteFile(fliInfoPath, []byte(info.String()), 0644); err != nil {
		os.Remove(fliInfoPath) //paranoid
		return CmdOutput{}, err
	}
	defer os.Remove(fliInfoPath)

	// list of files that we will add to archive
	files := []string{
		c.CfgParams.SQLMdsInitial,
		c.CfgParams.SQLMdsCurrent,
		filepath.Join(LogDir, CmdLogFilename),
		filepath.Join(LogDir, FliLogFilename),
		zfsDumpPath,
		zpoolDumpPath,
		fliInfoPath,
	}

	// setup gzip writer
	gzipWriter := gzip.NewWriter(archive)
	defer gzipWriter.Close()

	// setup tar writer
	tarWriter := tar.NewWriter(gzipWriter)
	defer tarWriter.Close()

	// add files to tar archive
	for _, file := range files {
		if err := addFileToTarball(tarWriter, file); err != nil {
			return CmdOutput{}, err
		}
	}

	tab = append(tab, []string{"Success. Generated archive: ", archivePath})
	tab = append(tab, []string{"Please email it to support@clusterhq.com"})

	return CmdOutput{Op: []CmdResult{{Tab: tab}}}, nil
}

// Info ...
func (c *Handler) Info(args []string) (Result, error) {
	tab := [][]string{}

	tab = append(tab, []string{"Version:", version.Version()})

	gitCommit := version.CommitID()
	if gitCommit != "" && len(gitCommit) == 40 {
		// git hash should be 40 char long
		tab = append(tab, []string{"Git commit:", gitCommit})
	}

	buildTime := version.BuildTime()
	if buildTime != "" {
		tab = append(tab, []string{"Built:", buildTime})
	}

	tab = append(tab, []string{"OS/Arch:", runtime.GOOS + "/" + runtime.GOARCH})

	if c.CfgParams.FlockerHubURL == "" {
		// FlockerHubURL is not set so just use the default
		c.CfgParams.FlockerHubURL = version.FlockerHubURL()
	}
	tab = append(tab, []string{"FlockerHub URL:", c.CfgParams.FlockerHubURL})

	if c.CfgParams.AuthTokenFile != "" {
		tab = append(tab, []string{"Auth Token File:", c.CfgParams.AuthTokenFile})
	}

	if c.CfgParams.Zpool != "" {
		tab = append(tab, []string{"ZPOOL:", c.CfgParams.Zpool})
	}

	if store, err := getStorage(c.CfgParams.Zpool); err == nil { // Error here is ignored
		zfsVer := store.Version()
		if zfsVer != "" {
			tab = append(tab, []string{"ZFS Version:", zfsVer})
		}
	}

	return CmdOutput{Op: []CmdResult{{Tab: tab}}}, nil
}

// NewHandler ...
func NewHandler(params ConfigParams, cfgFile, mdsCurr, mdsInit string) *Handler {
	return &Handler{
		CfgParams:      params,
		ConfigFile:     cfgFile,
		MdsPathCurrent: mdsCurr,
		MdsPathInitial: mdsInit,
	}
}
