// Copyright 2017 The Nakama Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"database/sql"
	"errors"

	"github.com/lib/pq"
	"github.com/satori/go.uuid"
	"github.com/uber-go/zap"
)

func (p *pipeline) querySocialGraph(logger zap.Logger, filterQuery string, params []interface{}) ([]*User, error) {
	users := []*User{}

	query := `
SELECT id, handle, fullname, avatar_url,
	lang, location, timezone, metadata,
	created_at, users.updated_at, last_online_at
FROM users ` + filterQuery

	rows, err := p.db.Query(query, params...)
	if err != nil {
		logger.Error("Could not execute social graph query", zap.String("query", query), zap.Error(err))
		return nil, err
	}
	defer rows.Close()

	var id sql.RawBytes
	var handle sql.NullString
	var fullname sql.NullString
	var avatarURL sql.NullString
	var lang sql.NullString
	var location sql.NullString
	var timezone sql.NullString
	var metadata sql.RawBytes
	var createdAt sql.NullInt64
	var updatedAt sql.NullInt64
	var lastOnlineAt sql.NullInt64

	for rows.Next() {
		err = rows.Scan(&id, &handle, &fullname, &avatarURL, &lang, &location, &timezone, &metadata, &createdAt, &updatedAt, &lastOnlineAt)
		if err != nil {
			logger.Error("Could not execute social graph query", zap.Error(err))
			return nil, err
		}

		users = append(users, &User{
			Id:           id,
			Handle:       handle.String,
			Fullname:     fullname.String,
			AvatarUrl:    avatarURL.String,
			Lang:         lang.String,
			Location:     location.String,
			Timezone:     timezone.String,
			Metadata:     metadata,
			CreatedAt:    createdAt.Int64,
			UpdatedAt:    updatedAt.Int64,
			LastOnlineAt: lastOnlineAt.Int64,
		})
	}
	if err = rows.Err(); err != nil {
		logger.Error("Could not execute social graph query", zap.Error(err))
		return nil, err
	}

	return users, nil
}

func (p *pipeline) addFacebookFriends(logger zap.Logger, userID []byte, accessToken string) {
	var tx *sql.Tx
	var err error

	defer func() {
		if err != nil {
			logger.Error("Could not import friends from Facebook", zap.Error(err))
			if tx != nil {
				err = tx.Rollback()
				if err != nil {
					logger.Error("Could not rollback transaction", zap.Error(err))
				}
			}
		} else {
			if tx != nil {
				err = tx.Commit()
				if err != nil {
					logger.Error("Could not commit transaction", zap.Error(err))
				} else {
					logger.Info("Imported friends")
				}
			}
		}
	}()

	fbFriends, err := p.socialClient.GetFacebookFriends(accessToken)
	if err != nil {
		return
	}

	tx, err = p.db.Begin()
	if err != nil {
		return
	}

	friendAddedCounter := 0
	for _, fbFriend := range fbFriends {
		var friendID sql.RawBytes
		err = tx.QueryRow("SELECT id FROM users WHERE facebook_id = $1", fbFriend.ID).Scan(&friendID)
		if err != nil {
			return
		}

		updatedAt := nowMs()
		_, err = tx.Exec(`
INSERT INTO user_edge (source_id, position, updated_at, destination_id, state)
VALUES ($1, $2, $2, $3, 0), ($3, $2, $2, $1, 0)`,
			userID, updatedAt, friendID)
		if err != nil {
			return
		}

		friendAddedCounter++

		_, err = tx.Exec(`UPDATE user_edge_metadata SET count = count + 1, updated_at = $1 WHERE source_id = $2`, updatedAt, friendID)
	}

	_, err = tx.Exec(`UPDATE user_edge_metadata SET count = $1, updated_at = $2 WHERE source_id = $3`, friendAddedCounter, nowMs(), userID)
}

func (p *pipeline) getFriends(filterQuery string, userID []byte) ([]*Friend, error) {
	query := `
SELECT id, handle, fullname, avatar_url,
	lang, location, timezone, metadata,
	created_at, users.updated_at, last_online_at, state
FROM users, user_edge ` + filterQuery

	rows, err := p.db.Query(query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	friends := make([]*Friend, 0)

	for rows.Next() {
		var id sql.RawBytes
		var handle sql.NullString
		var fullname sql.NullString
		var avatarURL sql.NullString
		var lang sql.NullString
		var location sql.NullString
		var timezone sql.NullString
		var metadata sql.RawBytes
		var createdAt sql.NullInt64
		var updatedAt sql.NullInt64
		var lastOnlineAt sql.NullInt64
		var state sql.NullInt64

		err = rows.Scan(&id, &handle, &fullname, &avatarURL, &lang, &location, &timezone, &metadata, &createdAt, &updatedAt, &lastOnlineAt, &state)
		if err != nil {
			return nil, err
		}

		friends = append(friends, &Friend{
			User: &User{
				Id:           id,
				Handle:       handle.String,
				Fullname:     fullname.String,
				AvatarUrl:    avatarURL.String,
				Lang:         lang.String,
				Location:     location.String,
				Timezone:     timezone.String,
				Metadata:     metadata,
				CreatedAt:    createdAt.Int64,
				UpdatedAt:    updatedAt.Int64,
				LastOnlineAt: lastOnlineAt.Int64,
			},
			Type: state.Int64,
		})
	}

	return friends, nil
}

func (p *pipeline) friendAdd(l zap.Logger, session *session, envelope *Envelope) {
	addFriendRequest := envelope.GetFriendAdd()
	if len(addFriendRequest.UserId) == 0 {
		session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Error{&Error{Reason: "User ID must be present"}}})
		return
	}

	friendID, err := uuid.FromBytes(addFriendRequest.UserId)
	if err != nil {
		l.Warn("Could not add friend", zap.Error(err))
		session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Error{&Error{Reason: "Invalid User ID"}}})
		return
	}
	logger := l.With(zap.String("friend_id", friendID.String()))
	friendIDBytes := friendID.Bytes()

	if friendID.String() == session.userID.String() {
		logger.Warn("Cannot add self", zap.Error(err))
		session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Error{&Error{Reason: "Cannot add self"}}})
		return
	}

	tx, err := p.db.Begin()
	if err != nil {
		logger.Error("Could not add friend", zap.Error(err))
		session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Error{&Error{Reason: "Failed to add friend"}}})
		return
	}
	defer func() {
		if err != nil {
			logger.Error("Could not add friend", zap.Error(err))
			err = tx.Rollback()
			if err != nil {
				logger.Error("Could not rollback transaction", zap.Error(err))
			}

			session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Error{&Error{Reason: "Failed to add friend"}}})
		} else {
			err = tx.Commit()
			if err != nil {
				logger.Error("Could not commit transaction", zap.Error(err))
				session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Error{&Error{Reason: "Failed to add friend"}}})
			} else {
				logger.Info("Added friend")
				session.Send(&Envelope{CollationId: envelope.CollationId})
			}
		}
	}()

	updatedAt := nowMs()
	res, err := tx.Exec("UPDATE user_edge SET state = 0, updated_at = $3 WHERE source_id = $1 AND destination_id = $2 AND state = 2", friendIDBytes, session.userID.Bytes(), updatedAt)
	if err != nil {
		return
	}

	state := 2
	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 1 {
		state = 0
	}

	res, err = tx.Exec(`
INSERT INTO user_edge (source_id, destination_id, state, position, updated_at)
SELECT $1, $2, $3, $4, $4
WHERE EXISTS (SELECT id FROM users WHERE id=$2)
	`, session.userID.Bytes(), friendIDBytes, state, updatedAt)
	if err != nil {
		return
	}

	rowsAffected, _ = res.RowsAffected()
	if rowsAffected == 0 {
		err = errors.New("Did not find friend ID in users table.")
		return
	}

	if state == 2 {
		_, err = tx.Exec("INSERT INTO user_edge (source_id, destination_id, state, position, updated_at) VALUES ($1, $2, $3, $4, $4)",
			friendIDBytes, session.userID.Bytes(), 1, updatedAt)

		if err != nil {
			return
		}

		_, err = tx.Exec("UPDATE user_edge_metadata SET count = count + 1, updated_at = $1 WHERE source_id = $2", updatedAt, friendIDBytes)

		if err != nil {
			return
		}
	}

	_, err = tx.Exec("UPDATE user_edge_metadata SET count = count + 1, updated_at = $1 WHERE source_id = $2", updatedAt, session.userID.Bytes())
}

func (p *pipeline) friendRemove(l zap.Logger, session *session, envelope *Envelope) {
	removeFriendRequest := envelope.GetFriendRemove()
	if len(removeFriendRequest.UserId) == 0 {
		session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Error{&Error{Reason: "User ID must be present"}}})
		return
	}

	friendID, err := uuid.FromBytes(removeFriendRequest.UserId)
	if err != nil {
		l.Warn("Could not add friend", zap.Error(err))
		session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Error{&Error{Reason: "Invalid User ID"}}})
		return
	}
	logger := l.With(zap.String("friend_id", friendID.String()))
	friendIDBytes := friendID.Bytes()

	if friendID.String() == session.userID.String() {
		logger.Warn("Cannot remove self", zap.Error(err))
		session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Error{&Error{Reason: "Cannot remove self"}}})
		return
	}

	tx, err := p.db.Begin()
	if err != nil {
		logger.Error("Could not remove friend", zap.Error(err))
		session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Error{&Error{Reason: "Failed to remove friend"}}})
		return
	}
	defer func() {
		if err != nil {
			logger.Error("Could not remove friend", zap.Error(err))
			err = tx.Rollback()
			if err != nil {
				logger.Error("Could not rollback transaction", zap.Error(err))
			}

			session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Error{&Error{Reason: "Failed to remove friend"}}})
		} else {
			err = tx.Commit()
			if err != nil {
				logger.Error("Could not commit transaction", zap.Error(err))
				session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Error{&Error{Reason: "Failed to remove friend"}}})
			} else {
				logger.Info("Removed friend")
				session.Send(&Envelope{CollationId: envelope.CollationId})
			}
		}
	}()

	updatedAt := nowMs()

	res, err := tx.Exec("DELETE FROM user_edge WHERE source_id = $1 AND destination_id = $2", session.userID.Bytes(), friendIDBytes)
	rowsAffected, _ := res.RowsAffected()
	if err == nil && rowsAffected > 0 {
		_, err = tx.Exec("UPDATE user_edge_metadata SET count = count - 1, updated_at = $2 WHERE source_id = $1", session.userID.Bytes(), updatedAt)
	}

	if err != nil {
		return
	}

	res, err = tx.Exec("DELETE FROM user_edge WHERE source_id = $1 AND destination_id = $2", friendIDBytes, session.userID.Bytes())
	rowsAffected, _ = res.RowsAffected()
	if err == nil && rowsAffected > 0 {
		_, err = tx.Exec("UPDATE user_edge_metadata SET count = count - 1, updated_at = $2 WHERE source_id = $1", friendIDBytes, updatedAt)
	}
}

func (p *pipeline) friendBlock(l zap.Logger, session *session, envelope *Envelope) {
	blockUserRequest := envelope.GetFriendBlock()
	if len(blockUserRequest.UserId) == 0 {
		session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Error{&Error{Reason: "User ID must be present"}}})
		return
	}

	userID, err := uuid.FromBytes(blockUserRequest.UserId)
	if err != nil {
		l.Warn("Could not block user", zap.Error(err))
		session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Error{&Error{Reason: "Invalid User ID"}}})
		return
	}
	logger := l.With(zap.String("user_id", userID.String()))
	userIDBytes := userID.Bytes()

	if userID.String() == session.userID.String() {
		logger.Warn("Cannot block self", zap.Error(err))
		session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Error{&Error{Reason: "Cannot block self"}}})
		return
	}

	tx, err := p.db.Begin()
	if err != nil {
		logger.Error("Could not block user", zap.Error(err))
		session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Error{&Error{Reason: "Failed to remove friend"}}})
		return
	}
	defer func() {
		if err != nil {
			if _, ok := err.(*pq.Error); ok {
				logger.Error("Could not block user", zap.Error(err))
			} else {
				logger.Warn("Could not block user", zap.Error(err))
			}
			err = tx.Rollback()
			if err != nil {
				logger.Error("Could not rollback transaction", zap.Error(err))
			}

			session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Error{&Error{Reason: "Could not block user"}}})
		} else {
			err = tx.Commit()
			if err != nil {
				logger.Error("Could not commit transaction", zap.Error(err))
				session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Error{&Error{Reason: "Could not block user"}}})
			} else {
				logger.Info("User blocked")
				session.Send(&Envelope{CollationId: envelope.CollationId})
			}
		}
	}()

	res, err := tx.Exec("UPDATE user_edge SET state = 3, updated_at = $3 WHERE source_id = $1 AND destination_id = $2",
		session.userID.Bytes(), userIDBytes, nowMs())

	if err != nil {
		return
	}

	if rowsAffected, _ := res.RowsAffected(); rowsAffected == 0 {
		err = errors.New("Could not block user. User ID may not exist")
		return
	}

	// Delete opposite relationship if user hasn't blocked you already
	res, err = tx.Exec("DELETE FROM user_edge WHERE source_id = $1 AND destination_id = $2 AND state != 3",
		userIDBytes, session.userID.Bytes())

	if err != nil {
		return
	}

	if rowsAffected, _ := res.RowsAffected(); rowsAffected == 1 {
		_, err = tx.Exec("UPDATE user_edge_metadata SET count = count - 1, updated_at = $2 WHERE source_id = $1", userIDBytes, nowMs())
	}
}

func (p *pipeline) friendsList(logger zap.Logger, session *session, envelope *Envelope) {
	friends, err := p.getFriends("WHERE id = destination_id AND source_id = $1", session.userID.Bytes())
	if err != nil {
		logger.Error("Could not get friends", zap.Error(err))
		session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Error{&Error{Reason: "Could not get friends"}}})
		return
	}

	session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Friends{Friends: &TFriends{Friends: friends}}})
}
