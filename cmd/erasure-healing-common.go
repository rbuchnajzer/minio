// Copyright (c) 2015-2021 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"bytes"
	"context"
	"time"

	"github.com/minio/madmin-go"
)

// commonTime returns a maximally occurring time from a list of time.
func commonTime(modTimes []time.Time) (modTime time.Time) {
	timeOccurenceMap := make(map[int64]int, len(modTimes))
	// Ignore the uuid sentinel and count the rest.
	for _, t := range modTimes {
		if t.Equal(timeSentinel) {
			continue
		}
		timeOccurenceMap[t.UnixNano()]++
	}

	var maxima int // Counter for remembering max occurrence of elements.

	// Find the common cardinality from previously collected
	// occurrences of elements.
	for nano, count := range timeOccurenceMap {
		t := time.Unix(0, nano).UTC()
		if count > maxima || (count == maxima && t.After(modTime)) {
			maxima = count
			modTime = t
		}
	}

	// Return the collected common modTime.
	return modTime
}

// Beginning of unix time is treated as sentinel value here.
var timeSentinel = time.Unix(0, 0).UTC()

// Boot modTimes up to disk count, setting the value to time sentinel.
func bootModtimes(diskCount int) []time.Time {
	modTimes := make([]time.Time, diskCount)
	// Boots up all the modtimes.
	for i := range modTimes {
		modTimes[i] = timeSentinel
	}
	return modTimes
}

// Extracts list of times from FileInfo slice and returns, skips
// slice elements which have errors.
func listObjectModtimes(partsMetadata []FileInfo, errs []error) (modTimes []time.Time) {
	modTimes = bootModtimes(len(partsMetadata))
	for index, metadata := range partsMetadata {
		if errs[index] != nil {
			continue
		}
		// Once the file is found, save the uuid saved on disk.
		modTimes[index] = metadata.ModTime
	}
	return modTimes
}

func filterOnlineDisksInplace(fi FileInfo, partsMetadata []FileInfo, onlineDisks []StorageAPI) {
	for i, meta := range partsMetadata {
		if fi.XLV1 == meta.XLV1 {
			continue
		}
		onlineDisks[i] = nil
	}
}

// Notes:
// There are 5 possible states a disk could be in,
// 1. __online__             - has the latest copy of xl.meta - returned by listOnlineDisks
//
// 2. __offline__            - err == errDiskNotFound
//
// 3. __availableWithParts__ - has the latest copy of xl.meta and has all
//                             parts with checksums matching; returned by disksWithAllParts
//
// 4. __outdated__           - returned by outDatedDisk, provided []StorageAPI
//                             returned by diskWithAllParts is passed for latestDisks.
//    - has an old copy of xl.meta
//    - doesn't have xl.meta (errFileNotFound)
//    - has the latest xl.meta but one or more parts are corrupt
//
// 5. __missingParts__       - has the latest copy of xl.meta but has some parts
// missing.  This is identified separately since this may need manual
// inspection to understand the root cause. E.g, this could be due to
// backend filesystem corruption.

// listOnlineDisks - returns
// - a slice of disks where disk having 'older' xl.meta (or nothing)
// are set to nil.
// - latest (in time) of the maximally occurring modTime(s).
func listOnlineDisks(disks []StorageAPI, partsMetadata []FileInfo, errs []error) (onlineDisks []StorageAPI, modTime time.Time) {
	onlineDisks = make([]StorageAPI, len(disks))

	// List all the file commit ids from parts metadata.
	modTimes := listObjectModtimes(partsMetadata, errs)

	// Reduce list of UUIDs to a single common value.
	modTime = commonTime(modTimes)

	// Create a new online disks slice, which have common uuid.
	for index, t := range modTimes {
		if partsMetadata[index].IsValid() && t.Equal(modTime) {
			onlineDisks[index] = disks[index]
		} else {
			onlineDisks[index] = nil
		}
	}

	return onlineDisks, modTime
}

// Returns the latest updated FileInfo files and error in case of failure.
func getLatestFileInfo(ctx context.Context, partsMetadata []FileInfo, errs []error) (FileInfo, error) {
	// There should be atleast half correct entries, if not return failure
	reducedErr := reduceReadQuorumErrs(ctx, errs, objectOpIgnoredErrs, len(partsMetadata)/2)
	if reducedErr != nil {
		return FileInfo{}, reducedErr
	}

	// List all the file commit ids from parts metadata.
	modTimes := listObjectModtimes(partsMetadata, errs)

	// Count all latest updated FileInfo values
	var count int
	var latestFileInfo FileInfo

	// Reduce list of UUIDs to a single common value - i.e. the last updated Time
	modTime := commonTime(modTimes)

	if modTime.IsZero() || modTime.Equal(timeSentinel) {
		return FileInfo{}, errErasureReadQuorum
	}

	// Interate through all the modTimes and count the FileInfo(s) with latest time.
	for index, t := range modTimes {
		if partsMetadata[index].IsValid() && t.Equal(modTime) {
			latestFileInfo = partsMetadata[index]
			count++
		}
	}

	if !latestFileInfo.IsValid() {
		return FileInfo{}, errErasureReadQuorum
	}

	if count < latestFileInfo.Erasure.DataBlocks {
		return FileInfo{}, errErasureReadQuorum
	}

	return latestFileInfo, nil
}

// disksWithAllParts - This function needs to be called with
// []StorageAPI returned by listOnlineDisks. Returns,
//
// - disks which have all parts specified in the latest xl.meta.
//
// - slice of errors about the state of data files on disk - can have
//   a not-found error or a hash-mismatch error.
func disksWithAllParts(ctx context.Context, onlineDisks []StorageAPI, partsMetadata []FileInfo,
	errs []error, latestMeta FileInfo,
	bucket, object string, scanMode madmin.HealScanMode) ([]StorageAPI, []error) {

	availableDisks := make([]StorageAPI, len(onlineDisks))
	dataErrs := make([]error, len(onlineDisks))
	inconsistent := 0
	for i, meta := range partsMetadata {
		if !meta.IsValid() {
			// Since for majority of the cases erasure.Index matches with erasure.Distribution we can
			// consider the offline disks as consistent.
			continue
		}
		if !meta.Deleted {
			if len(meta.Erasure.Distribution) != len(onlineDisks) {
				// Erasure distribution seems to have lesser
				// number of items than number of online disks.
				inconsistent++
				continue
			}
			if meta.Erasure.Distribution[i] != meta.Erasure.Index {
				// Mismatch indexes with distribution order
				inconsistent++
			}
		}
	}

	erasureDistributionReliable := true
	if inconsistent > len(partsMetadata)/2 {
		// If there are too many inconsistent files, then we can't trust erasure.Distribution (most likely
		// because of bugs found in CopyObject/PutObjectTags) https://github.com/minio/minio/pull/10772
		erasureDistributionReliable = false
	}

	for i, onlineDisk := range onlineDisks {
		if errs[i] != nil {
			dataErrs[i] = errs[i]
			continue
		}
		if onlineDisk == OfflineDisk {
			dataErrs[i] = errDiskNotFound
			continue
		}

		meta := partsMetadata[i]
		if !meta.ModTime.Equal(latestMeta.ModTime) || meta.DataDir != latestMeta.DataDir {
			dataErrs[i] = errFileCorrupt
			partsMetadata[i] = FileInfo{}
			continue
		}

		if erasureDistributionReliable {
			if !meta.IsValid() {
				continue
			}

			if !meta.Deleted {
				if len(meta.Erasure.Distribution) != len(onlineDisks) {
					// Erasure distribution is not the same as onlineDisks
					// attempt a fix if possible, assuming other entries
					// might have the right erasure distribution.
					partsMetadata[i] = FileInfo{}
					dataErrs[i] = errFileCorrupt
					continue
				}

				// Since erasure.Distribution is trustable we can fix the mismatching erasure.Index
				if meta.Erasure.Distribution[i] != meta.Erasure.Index {
					partsMetadata[i] = FileInfo{}
					dataErrs[i] = errFileCorrupt
					continue
				}
			}
		}

		// Always check data, if we got it.
		if (len(meta.Data) > 0 || meta.Size == 0) && len(meta.Parts) > 0 {
			checksumInfo := meta.Erasure.GetChecksumInfo(meta.Parts[0].Number)
			dataErrs[i] = bitrotVerify(bytes.NewReader(meta.Data),
				int64(len(meta.Data)),
				meta.Erasure.ShardFileSize(meta.Size),
				checksumInfo.Algorithm,
				checksumInfo.Hash, meta.Erasure.ShardSize())
			if dataErrs[i] == nil {
				// All parts verified, mark it as all data available.
				availableDisks[i] = onlineDisk
			} else {
				// upon errors just make that disk's fileinfo invalid
				partsMetadata[i] = FileInfo{}
			}
			continue
		}

		meta.DataDir = latestMeta.DataDir
		switch scanMode {
		case madmin.HealDeepScan:
			// disk has a valid xl.meta but may not have all the
			// parts. This is considered an outdated disk, since
			// it needs healing too.
			if !meta.Deleted && !meta.IsRemote() {
				dataErrs[i] = onlineDisk.VerifyFile(ctx, bucket, object, meta)
			}
		case madmin.HealNormalScan:
			if !meta.Deleted && !meta.IsRemote() {
				dataErrs[i] = onlineDisk.CheckParts(ctx, bucket, object, meta)
			}
		}

		if dataErrs[i] == nil {
			// All parts verified, mark it as all data available.
			availableDisks[i] = onlineDisk
		} else {
			// upon errors just make that disk's fileinfo invalid
			partsMetadata[i] = FileInfo{}
		}
	}

	return availableDisks, dataErrs
}
