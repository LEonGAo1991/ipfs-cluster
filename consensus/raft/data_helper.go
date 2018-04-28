package raft

import (
	"fmt"
	"os"
	"path/filepath"
)

// RaftDataBackupKeep indicates the number of data folders we keep around
// after consensus.Clean() has been called.
var RaftDataBackupKeep = 5

// dataBackupHelper helps making and rotating backups from a folder.
// it will name them <folderName>.old.0, .old.1... and so on.
// when a new backup is made, the old.0 is renamed to old.1 and so on.
// when the RaftDataBackupKeep number is reached, the last is always
// discarded.
type dataBackupHelper struct {
	baseDir    string
	folderName string
}

func newDataBackupHelper(dataFolder string) *dataBackupHelper {
	return &dataBackupHelper{
		baseDir:    filepath.Dir(dataFolder),
		folderName: filepath.Base(dataFolder),
	}
}

func (dbh *dataBackupHelper) makeName(i int) string {
	return filepath.Join(dbh.baseDir, fmt.Sprintf("%s.old.%d", dbh.folderName, i))
}

func (dbh *dataBackupHelper) listBackups() []string {
	backups := []string{}
	for i := 0; i < RaftDataBackupKeep; i++ {
		name := dbh.makeName(i)
		if _, err := os.Stat(name); os.IsNotExist(err) {
			return backups
		}
		backups = append(backups, name)
	}
	return backups
}

func (dbh *dataBackupHelper) makeBackup() error {
	folder := filepath.Join(dbh.baseDir, dbh.folderName)
	if _, err := os.Stat(folder); os.IsNotExist(err) {
		// nothing to backup
		logger.Debug("nothing to backup")
		return nil
	}

	// make sure config folder exists
	err := os.MkdirAll(dbh.baseDir, 0700)
	if err != nil {
		return err
	}

	// list all backups in it
	backups := dbh.listBackups()
	// remove last / oldest. Ex. if max is five, remove name.old.4
	if len(backups) >= RaftDataBackupKeep {
		os.RemoveAll(backups[len(backups)-1])
	} else { // append new backup folder. Ex, if 2 exist: add name.old.2
		backups = append(backups, dbh.makeName(len(backups)))
	}

	// increase number for all backups folders.
	// If there are 3: 1->2, 0->1.
	// Note in all cases the last backup in the list does not exist
	// (either removed or not created, just added to this list)
	for i := len(backups) - 1; i > 0; i-- {
		err := os.Rename(backups[i-1], backups[i])
		if err != nil {
			return err
		}
	}

	// save new as name.old.0
	return os.Rename(filepath.Join(dbh.baseDir, dbh.folderName), dbh.makeName(0))
}
