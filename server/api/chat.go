package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"social-network/pkg/db/sqlite"
	"social-network/util"
	"strconv"
	"time"
)

type DirectChatRequest struct {
	UserId int `json:"userId"`
}

func CreateOrGetDirectChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req DirectChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	username, err := util.GetUsernameFromSession(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var currentUser struct {
		ID int
	}
	err = sqlite.DB.QueryRow("SELECT id FROM users WHERE username = ?", username).Scan(&currentUser.ID)
	if err != nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}

	// Check if the user is trying to create a chat with themselves
	if currentUser.ID == req.UserId {
		http.Error(w, "Cannot start a chat with yourself", http.StatusBadRequest)
		return
	}

	// Check if the user exists
	var userExists bool
	err = sqlite.DB.QueryRow("SELECT EXISTS (SELECT 1 FROM users WHERE id = ?)", req.UserId).Scan(&userExists)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	if !userExists {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}

	// Check if there's at least one follow relationship between users (either user following the other)
	var followExists bool
	err = sqlite.DB.QueryRow(`
        SELECT EXISTS (
            SELECT 1 FROM followers 
            WHERE ((follower_id = ? AND followed_id = ?) 
            OR (follower_id = ? AND followed_id = ?))
            AND status = 'accepted'
        )`,
		currentUser.ID, req.UserId, req.UserId, currentUser.ID,
	).Scan(&followExists)

	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	if !followExists {
		http.Error(w, "Cannot start chat: at least one user must follow the other", http.StatusForbidden)
		return
	}

	// Check if a direct chat already exists between these users
	var chatID int
	err = sqlite.DB.QueryRow(`
       SELECT c.id 
       FROM chats c
       JOIN user_chat_status ucs1 ON c.id = ucs1.chat_id AND ucs1.user_id = ?
       JOIN user_chat_status ucs2 ON c.id = ucs2.chat_id AND ucs2.user_id = ?
       WHERE c.type = 'direct'`,
		currentUser.ID, req.UserId,
	).Scan(&chatID)

	if err != nil {
		if err != sql.ErrNoRows {
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}

		// No existing chat, create a new one
		result, err := sqlite.DB.Exec(`
          INSERT INTO chats (type, created_at)
          VALUES ('direct', CURRENT_TIMESTAMP)`,
		)
		if err != nil {
			http.Error(w, "Failed to create chat", http.StatusInternalServerError)
			return
		}

		id, err := result.LastInsertId()
		if err != nil {
			http.Error(w, "Failed to get chat ID", http.StatusInternalServerError)
			return
		}
		chatID = int(id)

		// Add both users to the chat
		_, err = sqlite.DB.Exec(`
          INSERT INTO user_chat_status (user_id, chat_id)
          VALUES (?, ?), (?, ?)`,
			currentUser.ID, chatID, req.UserId, chatID,
		)
		if err != nil {
			http.Error(w, "Failed to add users to chat", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": chatID,
	})
}

// GetUserChats returns all chats for the authenticated user
func GetUserChats(w http.ResponseWriter, r *http.Request) {
	// Get authenticated user
	username, err := util.GetUsernameFromSession(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var userId int
	err = sqlite.DB.QueryRow("SELECT id FROM users WHERE username = ?", username).Scan(&userId)
	if err != nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}

	// Get all chats for the user - direct chats where users follow each other, and group chats where user is a member
	rows, err := sqlite.DB.Query(`
        SELECT
            c.id,
            c.type,
            COALESCE(
                (SELECT COUNT(*) FROM chat_messages m
                 JOIN user_chat_status ucs ON ucs.chat_id = m.chat_id AND ucs.user_id = ?
                 WHERE m.chat_id = c.id
                 AND (m.created_at > ucs.last_read_message_id OR ucs.last_read_message_id IS NULL)),
                0
            ) as unread_count,
            (SELECT content FROM chat_messages
             WHERE chat_id = c.id
             ORDER BY created_at DESC LIMIT 1) as last_message,
            (SELECT created_at FROM chat_messages
             WHERE chat_id = c.id
             ORDER BY created_at DESC LIMIT 1) as last_message_time
        FROM chats c
        JOIN user_chat_status ucs ON c.id = ucs.chat_id AND ucs.user_id = ?
        WHERE 
            -- For direct chats: Only show where users follow each other
            (c.type = 'direct' AND EXISTS (
                SELECT 1 
                FROM user_chat_status ucs2
                WHERE ucs2.chat_id = c.id AND ucs2.user_id != ?
                AND (
                    -- Either current user follows the other user
                    EXISTS (
                        SELECT 1 FROM followers 
                        WHERE follower_id = ? AND followed_id = ucs2.user_id AND status = 'accepted'
                    )
                    OR 
                    -- Or the other user follows the current user
                    EXISTS (
                        SELECT 1 FROM followers 
                        WHERE follower_id = ucs2.user_id AND followed_id = ? AND status = 'accepted'
                    )
                )
            ))
            OR 
            -- For group chats: Only show if user is a member of the group
            (c.type != 'direct' AND EXISTS (
                SELECT 1
                FROM groups g
                JOIN group_members gm ON g.id = gm.group_id
                WHERE g.chat_id = c.id AND gm.user_id = ?
            ))
        ORDER BY last_message_time DESC NULLS LAST
    `, userId, userId, userId, userId, userId, userId)

	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		log.Printf("Error getting user chats: %v", err)
		return
	}
	defer rows.Close()

	var chats []map[string]interface{}
	for rows.Next() {
		var chat struct {
			ID              int       `json:"id"`
			Type            string    `json:"type"`
			UnreadCount     int       `json:"unread_count"`
			LastMessage     string    `json:"last_message"`
			LastMessageTime time.Time `json:"last_message_time"`
		}

		var lastMessage, lastMessageTime sql.NullString

		if err := rows.Scan(
			&chat.ID,
			&chat.Type,
			&chat.UnreadCount,
			&lastMessage,
			&lastMessageTime,
		); err != nil {
			log.Printf("Error scanning chat: %v", err)
			continue
		}

		if lastMessage.Valid {
			chat.LastMessage = lastMessage.String
		}

		if lastMessageTime.Valid {
			chat.LastMessageTime, _ = time.Parse(time.RFC3339, lastMessageTime.String)
		}

		chatItem := map[string]interface{}{
			"id":           chat.ID,
			"type":         chat.Type,
			"unread_count": chat.UnreadCount,
		}

		if lastMessage.Valid {
			chatItem["last_message"] = lastMessage.String
		}

		if lastMessageTime.Valid {
			chatItem["last_message_time"] = lastMessageTime.String
		}

		// For direct chats, add the other user's info
		if chat.Type == "direct" {
			var otherUser struct {
				ID        int    `json:"id"`
				FirstName string `json:"first_name"`
				LastName  string `json:"last_name"`
				Username  string `json:"username"`
				Avatar    string `json:"avatar"`
			}

			err := sqlite.DB.QueryRow(`
                SELECT
                    u.id,
                    u.first_name,
                    u.last_name,
                    u.username,
                    u.avatar
                FROM users u
                JOIN user_chat_status ucs ON u.id = ucs.user_id
                WHERE ucs.chat_id = ? AND u.id != ?
                LIMIT 1
            `, chat.ID, userId).Scan(
				&otherUser.ID,
				&otherUser.FirstName,
				&otherUser.LastName,
				&otherUser.Username,
				&otherUser.Avatar,
			)

			if err == nil {
				chatItem["id"] = chat.ID
				chatItem["participant_id"] = otherUser.ID
				chatItem["first_name"] = otherUser.FirstName
				chatItem["last_name"] = otherUser.LastName
				chatItem["username"] = otherUser.Username
				chatItem["avatar"] = otherUser.Avatar
			}
		} else {
			// For group chats, add the group info
			var groupInfo struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Avatar      string `json:"avatar"`
			}

			err := sqlite.DB.QueryRow(`
                SELECT
                    title as name,
                    description,
                    '' as avatar
                FROM groups g
                WHERE g.chat_id = ? 
                LIMIT 1
            `, chat.ID).Scan(
				&groupInfo.Name,
				&groupInfo.Description,
				&groupInfo.Avatar,
			)

			if err == nil {
				chatItem["name"] = groupInfo.Name
				chatItem["description"] = groupInfo.Description
				chatItem["avatar"] = groupInfo.Avatar
			}
		}

		chats = append(chats, chatItem)
	}

	// Now get all users who don't have a chat yet but either the current user follows them or they follow the current user
	rows, err = sqlite.DB.Query(`
        SELECT
            u.id,
            u.first_name,
            u.last_name,
            u.username,
            u.avatar
        FROM users u
        WHERE u.id != ? AND u.id IN (
            -- Users who either follow or are followed by the current user
            SELECT f.followed_id 
            FROM followers f
            WHERE f.follower_id = ? 
            AND f.status = 'accepted'
            UNION
            SELECT f.follower_id 
            FROM followers f
            WHERE f.followed_id = ? 
            AND f.status = 'accepted'
        )
        -- Exclude users who already have a chat with current user
        AND u.id NOT IN (
            SELECT ucs2.user_id 
            FROM user_chat_status ucs1
            JOIN user_chat_status ucs2 ON ucs1.chat_id = ucs2.chat_id AND ucs2.user_id != ucs1.user_id
            WHERE ucs1.user_id = ? AND EXISTS (
                SELECT 1 FROM chats c 
                WHERE c.id = ucs1.chat_id AND c.type = 'direct'
            )
        )
    `, userId, userId, userId, userId)

	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		log.Printf("Error getting potential chat users: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var user struct {
			ID        int    `json:"id"`
			FirstName string `json:"first_name"`
			LastName  string `json:"last_name"`
			Username  string `json:"username"`
			Avatar    string `json:"avatar"`
		}

		if err := rows.Scan(
			&user.ID,
			&user.FirstName,
			&user.LastName,
			&user.Username,
			&user.Avatar,
		); err != nil {
			log.Printf("Error scanning user: %v", err)
			continue
		}

		// Add this user as a potential chat
		chatItem := map[string]interface{}{
			"id":             -user.ID, // Use negative ID to indicate it's a potential chat, not a real one yet
			"type":           "direct",
			"unread_count":   0,
			"participant_id": user.ID,
			"first_name":     user.FirstName,
			"last_name":      user.LastName,
			"username":       user.Username,
			"avatar":         user.Avatar,
			"potential":      true, // Flag to indicate this is a potential chat
		}

		chats = append(chats, chatItem)
	}

	if err := json.NewEncoder(w).Encode(chats); err != nil {
		http.Error(w, "Error encoding response", http.StatusInternalServerError)
		return
	}
}

// GetGroupChatMessages returns all messages for a group chat
func GetGroupChatMessages(w http.ResponseWriter, r *http.Request) {
	// Get authenticated user
	username, err := util.GetUsernameFromSession(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var userId int
	err = sqlite.DB.QueryRow("SELECT id FROM users WHERE username = ?", username).Scan(&userId)
	if err != nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}

	// Get chatId from URL
	chatIdStr := r.PathValue("chatId")
	chatId, err := strconv.Atoi(chatIdStr)
	if err != nil {
		http.Error(w, "Invalid chat ID", http.StatusBadRequest)
		return
	}

	// Check if this is actually a group chat
	var chatType string
	err = sqlite.DB.QueryRow("SELECT type FROM chats WHERE id = ?", chatId).Scan(&chatType)
	if err != nil {
		http.Error(w, "Chat not found", http.StatusNotFound)
		return
	}

	if chatType != "group" {
		http.Error(w, "Requested chat is not a group chat", http.StatusBadRequest)
		return
	}

	// Check if the user is a member of this chat
	var isMember bool
	err = sqlite.DB.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM user_chat_status WHERE user_id = ? AND chat_id = ?)",
		userId, chatId,
	).Scan(&isMember)

	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	if !isMember {
		http.Error(w, "You are not a member of this group chat", http.StatusForbidden)
		return
	}

	// Find the group associated with this chat (for additional info)
	var groupId int
	var groupName string
	_ = sqlite.DB.QueryRow(`
        SELECT id, title
        FROM groups
        WHERE chat_id = ?
    `, chatId).Scan(&groupId, &groupName)
	// Note: We continue even if the group query fails, as we care about messages

	// Fetch messages for this chat
	rows, err := sqlite.DB.Query(`
        SELECT 
            m.id,
            m.sender_id,
            u.first_name,
            u.last_name,
            u.username,
            COALESCE(u.avatar, '') as avatar,
            m.content,
            m.created_at
        FROM chat_messages m
        JOIN users u ON m.sender_id = u.id
        WHERE m.chat_id = ?
        ORDER BY m.created_at ASC
    `, chatId)

	if err != nil {
		http.Error(w, "Error fetching messages", http.StatusInternalServerError)
		log.Printf("Error fetching group chat messages: %v", err)
		return
	}
	defer rows.Close()

	// Process the messages
	var messages []map[string]interface{}
	for rows.Next() {
		var msg struct {
			ID        int       `json:"id"`
			SenderID  int       `json:"sender_id"`
			FirstName string    `json:"first_name"`
			LastName  string    `json:"last_name"`
			Username  string    `json:"username"`
			Avatar    string    `json:"avatar"`
			Content   string    `json:"content"`
			CreatedAt time.Time `json:"created_at"`
		}

		var createdAtStr string
		err := rows.Scan(
			&msg.ID,
			&msg.SenderID,
			&msg.FirstName,
			&msg.LastName,
			&msg.Username,
			&msg.Avatar,
			&msg.Content,
			&createdAtStr,
		)

		if err != nil {
			log.Printf("Error scanning message row: %v", err)
			continue
		}

		msg.CreatedAt, _ = time.Parse(time.RFC3339, createdAtStr)

		// Format message in the structure expected by the frontend
		messageItem := map[string]interface{}{
			"id":         msg.ID,
			"type":       "groupChat", // Matches MessageType.GROUP_CHAT in frontend
			"groupId":    chatId,
			"userId":     msg.SenderID,
			"content":    msg.Content,
			"createdAt":  createdAtStr,
			"userName":   fmt.Sprintf("%s %s", msg.FirstName, msg.LastName),
			"userAvatar": msg.Avatar,
		}

		messages = append(messages, messageItem)
	}

	// Update user's last read message timestamp
	_, err = sqlite.DB.Exec(`
        UPDATE user_chat_status
        SET last_read_message_id = (
            SELECT MAX(id) FROM chat_messages WHERE chat_id = ?
        )
        WHERE user_id = ? AND chat_id = ?
    `, chatId, userId, chatId)

	if err != nil {
		log.Printf("Error updating last read timestamp: %v", err)
		// Continue anyway, this is not critical
	}

	// Return messages with chat info
	response := map[string]interface{}{
		"chatId":    chatId,
		"messages":  messages,
		"groupId":   groupId,
		"groupName": groupName,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "Error encoding response", http.StatusInternalServerError)
	}
}

// GetChatParticipants returns all participants of a specific chat
func GetChatParticipants(w http.ResponseWriter, r *http.Request) {
	// Get authenticated user
	username, err := util.GetUsernameFromSession(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var userId int
	err = sqlite.DB.QueryRow("SELECT id FROM users WHERE username = ?", username).Scan(&userId)
	if err != nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}

	// Get chat ID from URL
	chatIdStr := r.PathValue("chatId")
	chatId, err := strconv.Atoi(chatIdStr)
	if err != nil {
		http.Error(w, "Invalid chat ID", http.StatusBadRequest)
		return
	}

	// Check if the user is a participant in this chat
	var isMember bool
	err = sqlite.DB.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM user_chat_status WHERE user_id = ? AND chat_id = ?)",
		userId, chatId,
	).Scan(&isMember)

	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	if !isMember {
		http.Error(w, "You are not a participant in this chat", http.StatusForbidden)
		return
	}

	// Check if this is a direct chat
	var chatType string
	err = sqlite.DB.QueryRow("SELECT type FROM chats WHERE id = ?", chatId).Scan(&chatType)
	if err != nil {
		http.Error(w, "Chat not found", http.StatusNotFound)
		return
	}

	// For direct chats, check if there's at least one follow relationship
	if chatType == "direct" {
		// Get the other participant
		var otherUserId int
		err = sqlite.DB.QueryRow(`
            SELECT user_id FROM user_chat_status 
            WHERE chat_id = ? AND user_id != ?
        `, chatId, userId).Scan(&otherUserId)
		if err != nil {
			http.Error(w, "Could not find other participant", http.StatusNotFound)
			return
		}

		// Check if there's at least one follow relationship (either user follows the other)
		var followExists bool
		err = sqlite.DB.QueryRow(`
            SELECT EXISTS (
                SELECT 1 FROM followers 
                WHERE ((follower_id = ? AND followed_id = ?) 
                OR (follower_id = ? AND followed_id = ?))
                AND status = 'accepted'
            )
        `, userId, otherUserId, otherUserId, userId).Scan(&followExists)

		if err != nil {
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}

		if !followExists {
			http.Error(w, "Cannot view chat: at least one user must follow the other", http.StatusForbidden)
			return
		}
	}

	participantsQuery := `
        SELECT 
            u.id, 
            u.username, 
            u.first_name, 
            u.last_name, 
            u.avatar,
            COALESCE((SELECT status FROM followers WHERE follower_id = ? AND followed_id = u.id), 'none') as follow_status,
            COALESCE((SELECT status FROM followers WHERE follower_id = u.id AND followed_id = ?), 'none') as followed_status
        FROM users u
        JOIN user_chat_status ucs ON u.id = ucs.user_id
        WHERE ucs.chat_id = ?
    `

	rows, err := sqlite.DB.Query(participantsQuery, userId, userId, chatId)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var participants []map[string]interface{}
	for rows.Next() {
		var participant struct {
			ID             int    `json:"id"`
			Username       string `json:"username"`
			FirstName      string `json:"first_name"`
			LastName       string `json:"last_name"`
			Avatar         string `json:"avatar"`
			FollowStatus   string `json:"follow_status"`
			FollowedStatus string `json:"followed_status"`
		}

		if err := rows.Scan(
			&participant.ID,
			&participant.Username,
			&participant.FirstName,
			&participant.LastName,
			&participant.Avatar,
			&participant.FollowStatus,
			&participant.FollowedStatus,
		); err != nil {
			log.Printf("Error scanning participant: %v", err)
			continue
		}

		participantMap := map[string]interface{}{
			"id":              participant.ID,
			"username":        participant.Username,
			"first_name":      participant.FirstName,
			"last_name":       participant.LastName,
			"avatar":          participant.Avatar,
			"follow_status":   participant.FollowStatus,
			"followed_status": participant.FollowedStatus,
		}

		participants = append(participants, participantMap)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(participants); err != nil {
		http.Error(w, "Error encoding response", http.StatusInternalServerError)
		return
	}
}

// MarkChatAsRead marks all messages in a chat as read for the current user
func MarkChatAsRead(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Get chat ID from URL
	chatIDStr := r.PathValue("id")
	chatID, err := strconv.Atoi(chatIDStr)
	if err != nil {
		http.Error(w, "Invalid chat ID", http.StatusBadRequest)
		return
	}

	// Get current user from session
	username, err := util.GetUsernameFromSession(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var userID int
	err = sqlite.DB.QueryRow("SELECT id FROM users WHERE username = ?", username).Scan(&userID)
	if err != nil {
		http.Error(w, "Failed to get user information", http.StatusInternalServerError)
		return
	}

	// Start a transaction
	tx, err := sqlite.DB.Begin()
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	// Verify user is part of this chat
	var chatExists bool
	err = tx.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM user_chat_status 
			WHERE chat_id = ? AND user_id = ?
		)
	`, chatID, userID).Scan(&chatExists)

	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	if !chatExists {
		http.Error(w, "Chat not found or you don't have access", http.StatusNotFound)
		return
	}

	// Get the ID of the latest message in this chat
	var latestMessageID int
	err = tx.QueryRow(`
		SELECT COALESCE(MAX(id), 0) 
		FROM chat_messages 
		WHERE chat_id = ?
	`, chatID).Scan(&latestMessageID)

	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	// Update the user_chat_status to mark all messages as read
	_, err = tx.Exec(`
		UPDATE user_chat_status 
		SET last_read_message_id = ? 
		WHERE chat_id = ? AND user_id = ?
	`, latestMessageID, chatID, userID)

	if err != nil {
		http.Error(w, "Failed to update chat status", http.StatusInternalServerError)
		return
	}

	// Also mark any notifications related to this chat as read
	_, err = tx.Exec(`
		UPDATE notifications 
		SET is_read = true 
		WHERE user_id = ? AND type = 'message' AND 
		FROM_USER_ID IN (
			SELECT user_id FROM user_chat_status 
			WHERE chat_id = ? AND user_id != ?
		)
	`, userID, chatID, userID)

	if err != nil {
		http.Error(w, "Failed to update notifications", http.StatusInternalServerError)
		return
	}

	// Commit the transaction
	if err = tx.Commit(); err != nil {
		http.Error(w, "Failed to save changes", http.StatusInternalServerError)
		return
	}

	// Return success response
	response := map[string]interface{}{
		"chatId":      chatID,
		"unreadCount": 0,
		"success":     true,
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "Error encoding response", http.StatusInternalServerError)
	}
}
