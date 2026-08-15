package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

// Config holds environment configurations
type Config struct {
	DatabaseURL           string
	OpenRouterAPIKey      string
	OpenRouterModel       string
	Port                  string
	EngineToken           string
	AdapterToken          string
	CaseLogConversationID string
	InactivityTimeout     time.Duration
}

// MessageRequest is the incoming payload from the Node.js channel-adapter
type MessageRequest struct {
	ConversationID string `json:"conversation_id"`
	Channel        string `json:"channel"`
	Text           string `json:"text"`
	SenderRef      string `json:"sender_ref"`
}

// MessageResponse is returned to the Node.js channel-adapter
type MessageResponse struct {
	ReplyText string `json:"reply_text"`
}

// LLM structs for parsing responses
type LLMClaim struct {
	ClaimType  string `json:"claim_type"`
	ValueText  string `json:"value_text"`
	Confidence string `json:"confidence"`
}

type ClaimExtractionResult struct {
	Claims        []LLMClaim `json:"claims"`
	IsRefusal     bool       `json:"is_refusal"`
	IsInsult      bool       `json:"is_insult"`
	RefusalReason string     `json:"refusal_reason"`
	InsultReason  string     `json:"insult_reason"`
}

type CrossCheckResult struct {
	TargetRole   string `json:"target_role"` // "A" or "B"
	QuestionText string `json:"question_text"`
}

type ProposalResult struct {
	ProposalText string `json:"proposal_text"`
}

type ConsentResult struct {
	Consent string `json:"consent"` // "yes" or "no"
	Comment string `json:"comment"`
}

type ClaimRecord struct {
	ClaimType  string `json:"claim_type"`
	ValueText  string `json:"value_text"`
	Confidence string `json:"confidence"`
}

type ReplyDecision int

const (
	DecisionUnknown ReplyDecision = iota
	DecisionYes
	DecisionNo
)

func main() {
	// Load .env file
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, relying on environment variables")
	}

	config := &Config{
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		OpenRouterAPIKey:      os.Getenv("OPENROUTER_API_KEY"),
		OpenRouterModel:       os.Getenv("OPENROUTER_MODEL"),
		Port:                  os.Getenv("PORT"),
		EngineToken:           os.Getenv("ENGINE_TOKEN"),
		AdapterToken:          os.Getenv("ADAPTER_TOKEN"),
		CaseLogConversationID: os.Getenv("CASE_LOG_CONVERSATION_ID"),
		InactivityTimeout:     30 * time.Minute,
	}

	if config.Port == "" {
		config.Port = "8080"
	}
	if config.OpenRouterModel == "" {
		config.OpenRouterModel = "anthropic/claude-3.5-haiku"
	}
	if raw := os.Getenv("INACTIVITY_TIMEOUT"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			config.InactivityTimeout = parsed
		}
	}

	if config.DatabaseURL == "" || config.OpenRouterAPIKey == "" {
		log.Fatal("DATABASE_URL and OPENROUTER_API_KEY must be set")
	}

	// Connect to database pool
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, config.DatabaseURL)
	if err != nil {
		log.Fatalf("Unable to connect to database: %v\n", err)
	}
	defer pool.Close()

	// Verify connection
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("Database ping failed: %v\n", err)
	}
	if err := ensureRuntimeSchema(ctx, pool); err != nil {
		log.Fatalf("Database migration failed: %v\n", err)
	}
	log.Println("Database connection established successfully")
	go runOutboxWorker(ctx, pool)
	go runInactivityWorker(ctx, pool, config.InactivityTimeout)

	// Set up router
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	http.HandleFunc("/message", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if config.EngineToken != "" && r.Header.Get("Authorization") != "Bearer "+config.EngineToken {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		var req MessageRequest
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.ConversationID) == "" || strings.TrimSpace(req.Channel) == "" || strings.TrimSpace(req.Text) == "" ||
			(req.Channel != "email" && req.Channel != "slack" && req.Channel != "telegram") {
			http.Error(w, "Missing or invalid message fields", http.StatusBadRequest)
			return
		}

		log.Printf("[Inbound] channel=%s conversation=%s bytes=%d", req.Channel, redactIdentifier(req.ConversationID), len(req.Text))

		reply, err := processMessage(ctx, pool, config, req)
		if err != nil {
			log.Printf("Error processing message: %v\n", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(MessageResponse{ReplyText: reply})
	})

	server := &http.Server{Addr: ":" + config.Port, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	log.Printf("Go mediation engine listening on port %s...\n", config.Port)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Server failed to start: %v\n", err)
	}
}

// processMessage routes the incoming message based on conversation state
func processMessage(ctx context.Context, pool *pgxpool.Pool, cfg *Config, req MessageRequest) (string, error) {
	// Start a transaction
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	// Look up conversation link
	var caseID, partyID string
	var partyRole string
	err = tx.QueryRow(ctx, `
		SELECT case_id, party_id 
		FROM conversation_links 
		WHERE conversation_id = $1 AND channel = $2`,
		req.ConversationID, req.Channel).Scan(&caseID, &partyID)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No active case for this conversation channel. Check if message is a join code.
			cleanText := strings.ToUpper(strings.TrimSpace(req.Text))
			isJoinCode := len(cleanText) == 6 && isAlphanumericUpper(cleanText)

			if isJoinCode {
				var topicSummary, caseStatus string
				var unusedCaseID string
				err := tx.QueryRow(ctx, `
					SELECT id, status, topic_summary 
					FROM cases 
					WHERE join_code = $1 AND join_code_used_at IS NULL AND status = 'AWAITING_JOIN'
					FOR UPDATE`,
					cleanText).Scan(&unusedCaseID, &caseStatus, &topicSummary)

				if err == nil {
					// Join successful!
					newPartyID := generateUUID()
					_, err = tx.Exec(ctx, `
						INSERT INTO parties (id, case_id, role, display_ref) 
						VALUES ($1, $2, 'B', $3)`,
						newPartyID, unusedCaseID, req.SenderRef)
					if err != nil {
						return "", err
					}

					_, err = tx.Exec(ctx, `
						INSERT INTO conversation_links (id, conversation_id, channel, case_id, party_id) 
						VALUES ($1, $2, $3, $4, $5)`,
						generateUUID(), req.ConversationID, req.Channel, unusedCaseID, newPartyID)
					if err != nil {
						return "", err
					}

					_, err = tx.Exec(ctx, `
						UPDATE cases 
						SET status = 'INTAKE_B', join_code_used_at = NOW(), updated_at = NOW() 
						WHERE id = $1`,
						unusedCaseID)
					if err != nil {
						return "", err
					}

					// Log inbound message
					msgID := generateUUID()
					_, err = tx.Exec(ctx, `
						INSERT INTO messages_log (id, case_id, party_id, direction, channel, raw_text) 
						VALUES ($1, $2, $3, 'in', $4, $5)`,
						msgID, unusedCaseID, newPartyID, req.Channel, req.Text)
					if err != nil {
						return "", err
					}

					reply := fmt.Sprintf("Welcome to Shuttle Court. You joined a case regarding %s. What happened from your perspective?", topicSummary)

					// Log outbound message
					_, err = tx.Exec(ctx, `
						INSERT INTO messages_log (id, case_id, party_id, direction, channel, raw_text) 
						VALUES ($1, $2, $3, 'out', $4, $5)`,
						generateUUID(), unusedCaseID, newPartyID, req.Channel, reply)
					if err != nil {
						return "", err
					}

					if err := tx.Commit(ctx); err != nil {
						return "", err
					}
					return reply, nil
				}
			}

			if isJoinCode {
				return "That join code is invalid, expired, or not ready yet. Please check the code and try again.", nil
			}

			// Not a join attempt. Extract substantive claims before opening a case.
			llmRes, err := extractClaims(ctx, cfg, req.Text)
			if err != nil {
				return "", err
			}

			if llmRes.IsRefusal {
				return "I can only mediate everyday interpersonal or financial disagreements. If your safety is at risk, please contact appropriate emergency services or authorities directly.", nil
			}
			if llmRes.IsInsult {
				return "I can only carry substantive claims, not insults. Please describe your disagreement using checkable facts.", nil
			}
			if len(llmRes.Claims) == 0 {
				return "I can help mediate an everyday interpersonal or financial disagreement. Tell me briefly what the dispute is about.", nil
			}

			newCaseID, shortID, joinCode, err := insertCaseWithUniqueCodes(ctx, tx)
			if err != nil {
				return "", err
			}

			newPartyID := generateUUID()
			_, err = tx.Exec(ctx, `
				INSERT INTO parties (id, case_id, role, display_ref) 
				VALUES ($1, $2, 'A', $3)`,
				newPartyID, newCaseID, req.SenderRef)
			if err != nil {
				return "", err
			}

			_, err = tx.Exec(ctx, `
				INSERT INTO conversation_links (id, conversation_id, channel, case_id, party_id) 
				VALUES ($1, $2, $3, $4, $5)`,
				generateUUID(), req.ConversationID, req.Channel, newCaseID, newPartyID)
			if err != nil {
				return "", err
			}

			// Log inbound message
			msgID := generateUUID()
			_, err = tx.Exec(ctx, `
				INSERT INTO messages_log (id, case_id, party_id, direction, channel, raw_text) 
				VALUES ($1, $2, $3, 'in', $4, $5)`,
				msgID, newCaseID, newPartyID, req.Channel, req.Text)
			if err != nil {
				return "", err
			}

			// Save extracted claims
			for _, claim := range llmRes.Claims {
				_, err = tx.Exec(ctx, `
					INSERT INTO claims (id, case_id, party_id, claim_type, value_text, confidence, source_message_id) 
					VALUES ($1, $2, $3, $4, $5, $6, $7)`,
					generateUUID(), newCaseID, newPartyID, claim.ClaimType, claim.ValueText, claim.Confidence, msgID)
				if err != nil {
					return "", err
				}
			}

			reply := fmt.Sprintf("Hello, I am Docket, your Shuttle Court mediator. I have opened case **%s**. Share join code **%s** with the other party. What outcome would you like from mediation?", shortID, joinCode)

			// Log outbound message
			_, err = tx.Exec(ctx, `
				INSERT INTO messages_log (id, case_id, party_id, direction, channel, raw_text) 
				VALUES ($1, $2, $3, 'out', $4, $5)`,
				generateUUID(), newCaseID, newPartyID, req.Channel, reply)
			if err != nil {
				return "", err
			}

			if err := tx.Commit(ctx); err != nil {
				return "", err
			}
			return reply, nil
		}
		return "", err
	}

	// Link exists. Load case status & role
	var shortID, joinCode, status, topicSummary, deliveryIssue string
	var crossCheckRounds int
	err = tx.QueryRow(ctx, `
		SELECT short_id, join_code, status, topic_summary, cross_check_rounds, COALESCE(delivery_issue, '')
		FROM cases WHERE id = $1 FOR UPDATE`,
		caseID).Scan(&shortID, &joinCode, &status, &topicSummary, &crossCheckRounds, &deliveryIssue)
	if err != nil {
		return "", err
	}

	err = tx.QueryRow(ctx, `
		SELECT role FROM parties WHERE id = $1`,
		partyID).Scan(&partyRole)
	if err != nil {
		return "", err
	}

	// Log inbound message
	msgID := generateUUID()
	_, err = tx.Exec(ctx, `
		INSERT INTO messages_log (id, case_id, party_id, direction, channel, raw_text) 
		VALUES ($1, $2, $3, 'in', $4, $5)`,
		msgID, caseID, partyID, req.Channel, req.Text)
	if err != nil {
		return "", err
	}

	if isCounterpartWordsQuery(req.Text) {
		reply := "I can’t share the other party’s words. I can share only the neutral substance of the disagreement"
		if strings.TrimSpace(topicSummary) != "" {
			reply += ": " + strings.TrimSpace(topicSummary)
		}
		reply += "."
		if err := logOutbound(ctx, tx, caseID, partyID, req.Channel, reply); err != nil {
			return "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return reply, nil
	}

	// Handle explicit status queries only.
	if isStatusQuery(req.Text) {
		var reply string
		switch status {
		case "INTAKE":
			reply = "I am currently gathering details from you. Once you confirm them, we will wait for the other party."
		case "AWAITING_JOIN":
			reply = fmt.Sprintf("We are waiting for the other party to join. Please share the code **%s** with them.", joinCode)
		case "INTAKE_B":
			if partyRole == "A" {
				reply = "I am currently gathering information from the other party B. I will notify you once they confirm their details."
			} else {
				reply = "I am currently gathering details from you about the dispute."
			}
		case "CROSS_CHECK":
			reply = "I am currently cross-checking the details provided by both parties to identify and resolve any discrepancies."
		case "AWAITING_CONSENT":
			reply = "I have proposed a resolution and am waiting for consent from both parties."
		case "RESOLVED":
			reply = "This case has been successfully resolved."
		case "STALLED":
			reply = "This mediation has stalled."
		}
		if deliveryIssue != "" {
			reply += " A message delivery problem also needs attention."
		}

		if err := logOutbound(ctx, tx, caseID, partyID, req.Channel, reply); err != nil {
			return "", err
		}

		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return reply, nil
	}

	// Run guardrail checks & claim extraction
	llmRes, err := extractClaims(ctx, cfg, req.Text)
	if err != nil {
		return "", err
	}

	if llmRes.IsRefusal {
		return "I can only mediate everyday interpersonal or financial disagreements. If your safety is at risk, please contact appropriate emergency services or authorities directly.", nil
	}
	if llmRes.IsInsult {
		return "I can only carry substantive claims, not insults. Please describe your disagreement using checkable facts.", nil
	}

	// Replace each affected claim type once, preserving multiple values of the
	// same type extracted from one message (for example total and share amounts).
	deletedTypes := make(map[string]bool)
	for _, claim := range llmRes.Claims {
		if !deletedTypes[claim.ClaimType] {
			if _, err = tx.Exec(ctx, `DELETE FROM claims WHERE case_id = $1 AND party_id = $2 AND claim_type = $3`, caseID, partyID, claim.ClaimType); err != nil {
				return "", err
			}
			deletedTypes[claim.ClaimType] = true
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO claims (id, case_id, party_id, claim_type, value_text, confidence, source_message_id) 
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			generateUUID(), caseID, partyID, claim.ClaimType, claim.ValueText, claim.Confidence, msgID)
		if err != nil {
			return "", err
		}
	}

	// Main state machine
	var replyText string

	switch status {
	case "INTAKE":
		// check if replying to restatement
		var lastOutboundText string
		err = tx.QueryRow(ctx, `
			SELECT raw_text FROM messages_log 
			WHERE case_id = $1 AND party_id = $2 AND direction = 'out' 
			ORDER BY created_at DESC LIMIT 1`,
			caseID, partyID).Scan(&lastOutboundText)

		isReplyingToRestatement := err == nil && (strings.Contains(lastOutboundText, "is that right?") || strings.Contains(lastOutboundText, "is this correct?"))

		if isReplyingToRestatement {
			decision := classifyReplyDecision(req.Text)
			if decision != DecisionYes {
				if decision == DecisionNo {
					replyText = "What should I correct in that summary?"
				} else {
					replyText = "Please reply YES if that summary is correct, or NO if it needs correction."
				}
				break
			}
			// A has confirmed claims. Generate neutral topic summary
			topic, err := generateTopicSummary(ctx, cfg, caseID, partyID, tx)
			if err != nil {
				return "", err
			}

			_, err = tx.Exec(ctx, `
				UPDATE cases 
				SET status = 'AWAITING_JOIN', topic_summary = $2, updated_at = NOW() 
				WHERE id = $1`,
				caseID, topic)
			if err != nil {
				return "", err
			}

			replyText = fmt.Sprintf("Got it. I have recorded your confirmation. I am now waiting for the other party to join using the code: **%s**.", joinCode)
		} else {
			// Ask clarifying question or generate restatement
			var questionCount int
			err = tx.QueryRow(ctx, `
				SELECT COUNT(*) FROM messages_log 
				WHERE case_id = $1 AND party_id = $2 AND direction = 'out'`, caseID, partyID).Scan(&questionCount)
			if err != nil {
				return "", err
			}

			// We ask up to 3 clarifying questions (1 initial + 2 follow-ups)
			if questionCount < 3 {
				q, err := generateClarifyingQuestion(ctx, cfg, caseID, partyID, tx)
				if err != nil {
					return "", err
				}
				if q == "READY" {
					// Generate restatement
					replyText, err = generateRestatement(ctx, cfg, caseID, partyID, tx)
				} else {
					replyText = q
				}
			} else {
				// Generate restatement
				replyText, err = generateRestatement(ctx, cfg, caseID, partyID, tx)
			}
		}

	case "AWAITING_JOIN":
		replyText = fmt.Sprintf("We are currently waiting for the other party to join. Please share the code **%s** with them.", joinCode)

	case "INTAKE_B":
		// check B replying to restatement
		var lastOutboundText string
		err = tx.QueryRow(ctx, `
			SELECT raw_text FROM messages_log 
			WHERE case_id = $1 AND party_id = $2 AND direction = 'out' 
			ORDER BY created_at DESC LIMIT 1`,
			caseID, partyID).Scan(&lastOutboundText)

		isReplyingToRestatement := err == nil && (strings.Contains(lastOutboundText, "is that right?") || strings.Contains(lastOutboundText, "is this correct?"))

		if isReplyingToRestatement {
			decision := classifyReplyDecision(req.Text)
			if decision != DecisionYes {
				if decision == DecisionNo {
					replyText = "What should I correct in that summary?"
				} else {
					replyText = "Please reply YES if that summary is correct, or NO if it needs correction."
				}
				break
			}
			// B confirmed! Move to CROSS_CHECK and immediately perform cross-check
			_, err = tx.Exec(ctx, `
				UPDATE cases SET status = 'CROSS_CHECK', updated_at = NOW() WHERE id = $1`,
				caseID)
			if err != nil {
				return "", err
			}

			replyText, err = runCrossCheckFlow(ctx, tx, cfg, caseID, partyID)
		} else {
			var questionCount int
			err = tx.QueryRow(ctx, `
				SELECT COUNT(*) FROM messages_log 
				WHERE case_id = $1 AND party_id = $2 AND direction = 'out'`, caseID, partyID).Scan(&questionCount)
			if err != nil {
				return "", err
			}

			if questionCount < 3 { // 1 initial B response + 2 follow-ups
				q, err := generateClarifyingQuestion(ctx, cfg, caseID, partyID, tx)
				if err != nil {
					return "", err
				}
				if q == "READY" {
					replyText, err = generateRestatement(ctx, cfg, caseID, partyID, tx)
				} else {
					replyText = q
				}
			} else {
				replyText, err = generateRestatement(ctx, cfg, caseID, partyID, tx)
			}
		}

	case "CROSS_CHECK":
		// This party responded to a cross-check query. Run cross-check again
		replyText, err = runCrossCheckFlow(ctx, tx, cfg, caseID, partyID)

	case "AWAITING_CONSENT":
		// This party responded to the proposal. Evaluate consent.
		replyText, err = handleConsentEvaluation(ctx, tx, cfg, caseID, partyID, msgID, req.Text)

	case "PROPOSE":
		replyText = "I am preparing the resolution proposal. I will send it when it is ready."

	default:
		replyText = "Mediation for this case has ended."
	}

	if err != nil {
		return "", err
	}

	// Log outbound response
	_, err = tx.Exec(ctx, `
		INSERT INTO messages_log (id, case_id, party_id, direction, channel, raw_text) 
		VALUES ($1, $2, $3, 'out', $4, $5)`,
		generateUUID(), caseID, partyID, req.Channel, replyText)
	if err != nil {
		return "", err
	}

	if err := tx.Commit(ctx); err != nil {
		return "", err
	}

	return replyText, nil
}

// runCrossCheckFlow performs claims comparison and manages contradictions
func runCrossCheckFlow(ctx context.Context, tx pgx.Tx, cfg *Config, caseID, currentPartyID string) (string, error) {
	claimsA, err := loadClaimRecords(ctx, tx, caseID, "A")
	if err != nil {
		return "", err
	}
	claimsB, err := loadClaimRecords(ctx, tx, caseID, "B")
	if err != nil {
		return "", err
	}
	targetRole, claimType := findDeterministicContradiction(claimsA, claimsB)
	if targetRole == "" {
		return initiateProposal(ctx, tx, cfg, caseID, currentPartyID)
	}

	column := "cross_check_rounds_a"
	if targetRole == "B" {
		column = "cross_check_rounds_b"
	}
	var rounds int
	err = tx.QueryRow(ctx, fmt.Sprintf(`
		UPDATE cases SET cross_check_rounds = cross_check_rounds + 1,
		%s = %s + 1, updated_at = NOW()
		WHERE id = $1 RETURNING %s`, column, column, column), caseID).Scan(&rounds)
	if err != nil {
		return "", err
	}
	if rounds > 2 {
		return stallCrossCheck(ctx, tx, caseID, currentPartyID)
	}

	questionText := "Can you confirm the exact date involved in the dispute?"
	if claimType == "amount" {
		questionText = "Can you double-check and confirm the exact amount involved?"
	}

	// We have a targeted follow-up question
	targetPartyID, targetConvID, targetChannel, err := getPartyDetailsByRole(ctx, tx, caseID, targetRole)
	if err != nil {
		return "", err
	}

	if targetPartyID == currentPartyID {
		return questionText, nil
	}

	if err := enqueueProactiveMessage(ctx, tx, caseID, targetPartyID, targetConvID, targetChannel, questionText); err != nil {
		return "", err
	}

	// Reply to current user that we are cross-checking
	return "Thank you. I am cross-checking details with the other party to clarify some points. I will notify you once done.", nil
}

func stallCrossCheck(ctx context.Context, tx pgx.Tx, caseID, currentPartyID string) (string, error) {
	if _, err := tx.Exec(ctx, `UPDATE cases SET status = 'STALLED', updated_at = NOW() WHERE id = $1`, caseID); err != nil {
		return "", err
	}
	role := "A"
	if err := tx.QueryRow(ctx, `SELECT role FROM parties WHERE id = $1`, currentPartyID).Scan(&role); err != nil {
		return "", err
	}
	counterpartRole := "B"
	if role == "B" {
		counterpartRole = "A"
	}
	partyID, convID, channel, err := getPartyDetailsByRole(ctx, tx, caseID, counterpartRole)
	if err == nil {
		msg := "Mediation has stalled because the conflicting details could not be reconciled within the clarification limit."
		if err := enqueueProactiveMessage(ctx, tx, caseID, partyID, convID, channel, msg); err != nil {
			return "", err
		}
	}
	return "Mediation has stalled because the conflicting details could not be reconciled within the clarification limit.", nil
}

// initiateProposal generates a resolution proposal and sends to both
func initiateProposal(ctx context.Context, tx pgx.Tx, cfg *Config, caseID, currentPartyID string) (string, error) {
	// Transition cases to PROPOSE
	_, err := tx.Exec(ctx, `
		UPDATE cases SET status = 'PROPOSE', updated_at = NOW() WHERE id = $1`,
		caseID)
	if err != nil {
		return "", err
	}

	// Load claims
	claimsA, _ := loadClaims(ctx, tx, caseID, "A")
	claimsB, _ := loadClaims(ctx, tx, caseID, "B")
	claimsSnapshot, err := buildClaimsSnapshot(ctx, tx, caseID)
	if err != nil {
		return "", err
	}

	systemPrompt := `You are Docket, the Shuttle Court AI mediator.
You are generating a resolution proposal based on these reconciled claims:
Party A:
` + claimsA + `

Party B:
` + claimsB + `

Create a fair, balanced, and extremely professional resolution proposal.
Write it in second-person or neutral third-person ("Party A will pay Party B...") so that the proposal text is identical for both.
Keep the proposal text concise, objective, and structured.
Do NOT use names of the parties.

Respond strictly in JSON format matching this schema:
{
  "proposal_text": "proposal details..."
}`

	var res ProposalResult
	err = callOpenRouterJSON(ctx, cfg, systemPrompt, "Generate a resolution proposal", &res)
	if err != nil {
		return "", err
	}

	// Save proposal
	proposalID := generateUUID()
	_, err = tx.Exec(ctx, `
		INSERT INTO proposals (id, case_id, version, proposal_text, generated_from) 
		VALUES ($1, $2, 1, $3, $4)`,
		proposalID, caseID, res.ProposalText, claimsSnapshot)
	if err != nil {
		return "", err
	}

	// Transition case to AWAITING_CONSENT
	_, err = tx.Exec(ctx, `
		UPDATE cases SET status = 'AWAITING_CONSENT', updated_at = NOW() WHERE id = $1`,
		caseID)
	if err != nil {
		return "", err
	}

	// Send proposal proactively to counterpart
	counterpartRole := "B"
	var currentPartyRole string
	if err := tx.QueryRow(ctx, "SELECT role FROM parties WHERE id = $1", currentPartyID).Scan(&currentPartyRole); err != nil {
		return "", err
	}
	if currentPartyRole == "B" {
		counterpartRole = "A"
	}

	cPartyID, cConvID, cChannel, err := getPartyDetailsByRole(ctx, tx, caseID, counterpartRole)
	if err == nil {
		proposalMsg := fmt.Sprintf("I have formulated a resolution proposal:\n\n%s\n\nDo you accept this proposal? Please reply YES or NO.", res.ProposalText)
		if err := enqueueProactiveMessage(ctx, tx, caseID, cPartyID, cConvID, cChannel, proposalMsg); err != nil {
			return "", err
		}
	}

	// Return proposal directly to current user
	return fmt.Sprintf("I have formulated a resolution proposal:\n\n%s\n\nDo you accept this proposal? Please reply YES or NO.", res.ProposalText), nil
}

// handleConsentEvaluation processes the user's vote on the proposal
func handleConsentEvaluation(ctx context.Context, tx pgx.Tx, cfg *Config, caseID, currentPartyID, sourceMessageID, userText string) (string, error) {
	// Load latest proposal
	var proposalID string
	var proposalText string
	var version int
	err := tx.QueryRow(ctx, `
		SELECT id, proposal_text, version 
		FROM proposals 
		WHERE case_id = $1 
		ORDER BY version DESC LIMIT 1`,
		caseID).Scan(&proposalID, &proposalText, &version)
	if err != nil {
		return "", err
	}
	decision := classifyReplyDecision(userText)
	if decision == DecisionUnknown {
		return "Please reply YES to accept the proposal, or NO followed by your concern.", nil
	}
	res := ConsentResult{Consent: "yes"}
	if decision == DecisionNo {
		res.Consent = "no"
		res.Comment = strings.TrimSpace(userText)
	}

	// Record consent
	consentID := generateUUID()
	_, err = tx.Exec(ctx, `
		INSERT INTO consents (id, proposal_id, party_id, decision, comment) 
		VALUES ($1, $2, $3, $4, $5) 
		ON CONFLICT (proposal_id, party_id) DO UPDATE 
		SET decision = EXCLUDED.decision, comment = EXCLUDED.comment`,
		consentID, proposalID, currentPartyID, res.Consent, res.Comment)
	if err != nil {
		return "", err
	}
	if res.Consent == "no" {
		_, err = tx.Exec(ctx, `UPDATE consents SET objection_claim_ids = COALESCE((
			SELECT jsonb_agg(id) FROM claims WHERE source_message_id = $2
		), '[]'::jsonb) WHERE proposal_id = $1 AND party_id = $3`, proposalID, sourceMessageID, currentPartyID)
		if err != nil {
			return "", err
		}
	}

	// Check if both consented
	var consentsCount int
	err = tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM consents 
		WHERE proposal_id = $1 AND decision = 'yes'`,
		proposalID).Scan(&consentsCount)
	if err != nil {
		return "", err
	}

	if consentsCount == 2 {
		// Both consented! Resolve case
		_, err = tx.Exec(ctx, `
			UPDATE cases 
			SET status = 'RESOLVED', resolved_at = NOW(), updated_at = NOW() 
			WHERE id = $1`,
			caseID)
		if err != nil {
			return "", err
		}

		// Proactively notify counterpart
		var currentPartyRole string
		if err := tx.QueryRow(ctx, "SELECT role FROM parties WHERE id = $1", currentPartyID).Scan(&currentPartyRole); err != nil {
			return "", err
		}
		counterpartRole := "B"
		if currentPartyRole == "B" {
			counterpartRole = "A"
		}
		cPartyID, cConvID, cChannel, err := getPartyDetailsByRole(ctx, tx, caseID, counterpartRole)
		if err == nil {
			msg := "Excellent. Both parties have consented. This case is now resolved. Thank you for using Shuttle Court."
			if err := enqueueProactiveMessage(ctx, tx, caseID, cPartyID, cConvID, cChannel, msg); err != nil {
				return "", err
			}
		}

		if cfg.CaseLogConversationID != "" {
			summary := fmt.Sprintf("SHUTTLE COURT CASE RESOLUTION LOG\nCase ID: %s\n\nProposal Accepted:\n%s", caseID, proposalText)
			if err := enqueueProactiveMessage(ctx, tx, caseID, currentPartyID, cfg.CaseLogConversationID, "email", summary); err != nil {
				return "", err
			}
		}

		return "Excellent. Both parties have consented. This case is now resolved. Thank you for using Shuttle Court.", nil
	}

	// Rejection case
	if res.Consent == "no" {
		if version >= 3 {
			// Stall the case
			_, err = tx.Exec(ctx, `
				UPDATE cases SET status = 'STALLED', updated_at = NOW() WHERE id = $1`,
				caseID)
			if err != nil {
				return "", err
			}

			// Proactively notify counterpart
			var currentPartyRole string
			if err := tx.QueryRow(ctx, "SELECT role FROM parties WHERE id = $1", currentPartyID).Scan(&currentPartyRole); err != nil {
				return "", err
			}
			counterpartRole := "B"
			if currentPartyRole == "B" {
				counterpartRole = "A"
			}
			cPartyID, cConvID, cChannel, err := getPartyDetailsByRole(ctx, tx, caseID, counterpartRole)
			if err == nil {
				msg := "Mediation has stalled because we could not reach an agreement."
				if err := enqueueProactiveMessage(ctx, tx, caseID, cPartyID, cConvID, cChannel, msg); err != nil {
					return "", err
				}
			}

			return "Mediation has stalled because we could not reach an agreement.", nil
		}

		// Create a revised proposal
		claimsA, _ := loadClaims(ctx, tx, caseID, "A")
		claimsB, _ := loadClaims(ctx, tx, caseID, "B")
		claimsSnapshot, err := buildClaimsSnapshot(ctx, tx, caseID)
		if err != nil {
			return "", err
		}

		objectionPrompt := `You are Docket, the Shuttle Court AI mediator.
Generate a revised proposal using only the structured claims below. One party raised a concern, which has already been converted into these claims. Never infer or reproduce private source wording.
Previous proposal: "` + proposalText + `"
Structured claims:
Party A:
` + claimsA + `

Party B:
` + claimsB + `

Generate a revised, fair proposal that addresses the objection if possible, or suggests a reasonable compromise.
Do NOT use names of the parties. Keep the text neutral and identical for both.

Respond strictly in JSON format matching this schema:
{
  "proposal_text": "revised proposal details..."
}`

		var newRes ProposalResult
		err = callOpenRouterJSON(ctx, cfg, objectionPrompt, "Generate revised proposal", &newRes)
		if err != nil {
			return "", err
		}

		// Save revised proposal
		newProposalID := generateUUID()
		_, err = tx.Exec(ctx, `
			INSERT INTO proposals (id, case_id, version, proposal_text, generated_from) 
			VALUES ($1, $2, $3, $4, $5)`,
			newProposalID, caseID, version+1, newRes.ProposalText, claimsSnapshot)
		if err != nil {
			return "", err
		}

		// Proactively notify counterpart (without leaking raw objection text)
		var currentPartyRole string
		if err := tx.QueryRow(ctx, "SELECT role FROM parties WHERE id = $1", currentPartyID).Scan(&currentPartyRole); err != nil {
			return "", err
		}
		counterpartRole := "B"
		if currentPartyRole == "B" {
			counterpartRole = "A"
		}
		cPartyID, cConvID, cChannel, err := getPartyDetailsByRole(ctx, tx, caseID, counterpartRole)
		if err == nil {
			revisedMsg := fmt.Sprintf("The other party had a concern about the proposal. Here is a revised version:\n\n%s\n\nDo you accept this revised proposal? Please reply YES or NO.", newRes.ProposalText)
			if err := enqueueProactiveMessage(ctx, tx, caseID, cPartyID, cConvID, cChannel, revisedMsg); err != nil {
				return "", err
			}
		}

		return fmt.Sprintf("I have formulated a revised proposal to address concerns:\n\n%s\n\nDo you accept this revised proposal? Please reply YES or NO.", newRes.ProposalText), nil
	}

	// Just recorded a YES consent, waiting for the other party
	return "Thank you for your response. I am waiting for the other party to vote on the proposal.", nil
}

// --- LLM client functions ---

func extractClaims(ctx context.Context, cfg *Config, text string) (*ClaimExtractionResult, error) {
	systemPrompt := `You are Docket, the Shuttle Court AI mediator. Your job is to extract checkable claims from the user's message.
A checkable claim is a specific fact, amount, date, event description, or outcome.

You must categorize each claim into one of these types:
- 'amount': any money amount mentioned. The value should be normalized to a numeric string (e.g., "340.00").
- 'date': any date or timeframe mentioned. The value should be normalized (e.g., "2026-03-14", "March", "last week").
- 'who_did_what': a neutral, objective, and short summary of what happened (e.g., "paid for March electricity bill", "did not pay rent").
- 'desired_outcome': what the user wants to resolve this dispute (e.g., "wants $170 back", "wants counterpart to pay half").
- 'other': any other factual claim.

For each claim, assign a confidence level:
- 'stated': if they clearly and explicitly state it (e.g. "I paid $340").
- 'vague': if it is mentioned but unclear or lacking detail (e.g. "I paid the bill" without amount, or "sometime last month" without date).
- 'confirmed': only use if they explicitly confirm a restated point.

Safety & Guardrails:
1. Set "is_refusal" to true if the message describes illegal activity, self-harm, physical violence, or threats. Do not extract claims.
2. Set "is_insult" to true if the message is purely an insult or toxic rant with no checkable factual claims. Do not extract claims.

Respond strictly in JSON format matching this schema:
{
  "claims": [
    {
      "claim_type": "amount"|"date"|"who_did_what"|"desired_outcome"|"other",
      "value_text": "extracted and normalized text",
      "confidence": "stated"|"vague"|"confirmed"
    }
  ],
  "is_refusal": false,
  "is_insult": false,
  "refusal_reason": "",
  "insult_reason": ""
}`

	var res ClaimExtractionResult
	err := callOpenRouterJSON(ctx, cfg, systemPrompt, text, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func generateTopicSummary(ctx context.Context, cfg *Config, caseID, partyID string, tx pgx.Tx) (string, error) {
	claims, err := loadClaims(ctx, tx, caseID, "A")
	if err != nil {
		return "", err
	}

	systemPrompt := `You are Docket, the Shuttle Court AI mediator.
Based on the following claims from the case creator, generate a neutral, extremely brief, 1-line summary of the dispute (e.g. "a dispute about a shared electricity bill from March").
Do not mention any names. Keep it objective and simple.

Respond strictly in JSON:
{
  "topic_summary": "extremely short summary"
}`

	var res struct {
		TopicSummary string `json:"topic_summary"`
	}
	err = callOpenRouterJSON(ctx, cfg, systemPrompt, claims, &res)
	if err != nil {
		return "", err
	}
	return res.TopicSummary, nil
}

func generateClarifyingQuestion(ctx context.Context, cfg *Config, caseID, partyID string, tx pgx.Tx) (string, error) {
	var role string
	err := tx.QueryRow(ctx, "SELECT role FROM parties WHERE id = $1", partyID).Scan(&role)
	if err != nil {
		return "", err
	}
	claims, err := loadClaims(ctx, tx, caseID, role)
	if err != nil {
		return "", err
	}

	systemPrompt := `You are Docket, the Shuttle Court AI mediator.
You are privately gathering claims from a party.
Based on their current claims, ask ONE simple, focused clarifying question to gather missing details (e.g. date, amount, or desired outcome).
If they have already provided a clear amount, date, and description of what happened, or if you have enough context, respond with "READY".
Never ask multiple questions at once. Keep the tone neutral and concise.

Respond strictly in JSON:
{
  "question": "clarifying question or READY"
}`

	var res struct {
		Question string `json:"question"`
	}
	err = callOpenRouterJSON(ctx, cfg, systemPrompt, claims, &res)
	if err != nil {
		return "", err
	}
	return res.Question, nil
}

func generateRestatement(ctx context.Context, cfg *Config, caseID, partyID string, tx pgx.Tx) (string, error) {
	var role string
	err := tx.QueryRow(ctx, "SELECT role FROM parties WHERE id = $1", partyID).Scan(&role)
	if err != nil {
		return "", err
	}
	claims, err := loadClaims(ctx, tx, caseID, role)
	if err != nil {
		return "", err
	}

	systemPrompt := `You are Docket, the Shuttle Court AI mediator.
Summarize the current party's claims to confirm your understanding before proceeding.
Use their claims to formulate a summary:
"So the dispute is about a [amount] [who_did_what] from [date] - is that right?"
Keep it neutral and simple.

Respond strictly in JSON:
{
  "restatement": "So the dispute is about... - is that right?"
}`

	var res struct {
		Restatement string `json:"restatement"`
	}
	err = callOpenRouterJSON(ctx, cfg, systemPrompt, claims, &res)
	if err != nil {
		return "", err
	}
	return res.Restatement, nil
}

// callOpenRouterJSON queries OpenRouter for a JSON response
func callOpenRouterJSON(ctx context.Context, cfg *Config, systemPrompt, userPrompt string, result interface{}) error {
	payload := map[string]interface{}{
		"model": cfg.OpenRouterModel,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"response_format": map[string]string{"type": "json_object"},
		"max_tokens":      1000,
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			log.Printf("[OpenRouter] Attempt %d failed. Retrying in %d ms...", attempt-1, attempt*300)
			time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
		}

		req, err := http.NewRequestWithContext(ctx, "POST", "https://openrouter.ai/api/v1/chat/completions", bytes.NewBuffer(bodyBytes))
		if err != nil {
			lastErr = err
			continue
		}

		req.Header.Set("Authorization", "Bearer "+cfg.OpenRouterAPIKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("HTTP-Referer", "http://localhost:8080")
		req.Header.Set("X-Title", "Shuttle Court")

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode >= 400 {
			resp.Body.Close()
			lastErr = fmt.Errorf("openrouter API error (status %d)", resp.StatusCode)
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		var openRouterResponse struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}

		if err := json.Unmarshal(respBody, &openRouterResponse); err != nil {
			lastErr = fmt.Errorf("failed to decode OpenRouter response: %w", err)
			continue
		}

		if len(openRouterResponse.Choices) == 0 {
			lastErr = errors.New("empty response choices from OpenRouter")
			continue
		}

		contentStr := openRouterResponse.Choices[0].Message.Content

		// Clean markdown JSON wrapping if present
		cleaned := strings.TrimSpace(contentStr)
		if strings.HasPrefix(cleaned, "```json") {
			cleaned = strings.TrimPrefix(cleaned, "```json")
			cleaned = strings.TrimSuffix(cleaned, "```")
		} else if strings.HasPrefix(cleaned, "```") {
			cleaned = strings.TrimPrefix(cleaned, "```")
			cleaned = strings.TrimSuffix(cleaned, "```")
		}
		cleaned = strings.TrimSpace(cleaned)

		if err := json.Unmarshal([]byte(cleaned), result); err != nil {
			log.Printf("Failed to decode model JSON on attempt %d", attempt)
			lastErr = fmt.Errorf("failed to decode model JSON: %w", err)
			continue
		}

		// Success!
		return nil
	}

	return fmt.Errorf("all OpenRouter attempts failed. Last error: %w", lastErr)
}

// --- Database Helper Functions ---

func loadClaims(ctx context.Context, tx pgx.Tx, caseID, roleFilter string) (string, error) {
	var rows pgx.Rows
	var err error
	if roleFilter != "" {
		rows, err = tx.Query(ctx, `
			SELECT c.claim_type, c.value_text, c.confidence 
			FROM claims c
			JOIN parties p ON c.party_id = p.id
			WHERE c.case_id = $1 AND p.role = $2`,
			caseID, roleFilter)
	} else {
		rows, err = tx.Query(ctx, `
			SELECT claim_type, value_text, confidence 
			FROM claims 
			WHERE case_id = $1`,
			caseID)
	}
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var sb strings.Builder
	for rows.Next() {
		var cType, val, conf string
		if err := rows.Scan(&cType, &val, &conf); err != nil {
			return "", err
		}
		sb.WriteString(fmt.Sprintf("- Type: %s, Value: %s, Confidence: %s\n", cType, val, conf))
	}
	return sb.String(), nil
}

func loadClaimRecords(ctx context.Context, tx pgx.Tx, caseID, role string) ([]ClaimRecord, error) {
	rows, err := tx.Query(ctx, `
		SELECT c.claim_type, c.value_text, c.confidence
		FROM claims c JOIN parties p ON p.id = c.party_id
		WHERE c.case_id = $1 AND p.role = $2
		ORDER BY c.claim_type, c.created_at`, caseID, role)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ClaimRecord
	for rows.Next() {
		var claim ClaimRecord
		if err := rows.Scan(&claim.ClaimType, &claim.ValueText, &claim.Confidence); err != nil {
			return nil, err
		}
		result = append(result, claim)
	}
	return result, rows.Err()
}

func buildClaimsSnapshot(ctx context.Context, tx pgx.Tx, caseID string) ([]byte, error) {
	a, err := loadClaimRecords(ctx, tx, caseID, "A")
	if err != nil {
		return nil, err
	}
	b, err := loadClaimRecords(ctx, tx, caseID, "B")
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string][]ClaimRecord{"party_a": a, "party_b": b})
}

func insertCaseWithUniqueCodes(ctx context.Context, tx pgx.Tx) (caseID, shortID, joinCode string, err error) {
	for attempt := 0; attempt < 8; attempt++ {
		caseID, shortID, joinCode = generateUUID(), generateCode(6), generateCode(6)
		err = tx.QueryRow(ctx, `INSERT INTO cases (id, short_id, join_code, status, topic_summary, cross_check_rounds)
			VALUES ($1, $2, $3, 'INTAKE', '', 0) ON CONFLICT DO NOTHING RETURNING id`, caseID, shortID, joinCode).Scan(&caseID)
		if err == nil {
			return caseID, shortID, joinCode, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", "", "", err
		}
	}
	return "", "", "", errors.New("could not allocate unique case codes")
}

func isStatusQuery(text string) bool {
	normalized := strings.Trim(strings.ToLower(strings.TrimSpace(text)), "?.! ")
	switch normalized {
	case "status", "case status", "what is the status", "what's the status", "where are we", "any update", "any updates":
		return true
	default:
		return false
	}
}

func isCounterpartWordsQuery(text string) bool {
	normalized := strings.ToLower(text)
	patterns := []string{"what did they say", "what did the other party say", "show me their message", "send me their message", "quote them"}
	for _, pattern := range patterns {
		if strings.Contains(normalized, pattern) {
			return true
		}
	}
	return false
}

func redactIdentifier(value string) string {
	if len(value) <= 8 {
		return "[redacted]"
	}
	return value[:4] + "…" + value[len(value)-4:]
}

func logOutbound(ctx context.Context, tx pgx.Tx, caseID, partyID, channel, messageText string) error {
	_, err := tx.Exec(ctx, `INSERT INTO messages_log (id, case_id, party_id, direction, channel, raw_text)
		VALUES ($1, $2, $3, 'out', $4, $5)`, generateUUID(), caseID, partyID, channel, messageText)
	return err
}

func classifyReplyDecision(text string) ReplyDecision {
	normalized := strings.ToLower(strings.TrimSpace(text))
	yesPattern := regexp.MustCompile(`^(yes|y|yeah|yep|accept|accepted|agree|agreed|correct)(\b|[.!])`)
	noPattern := regexp.MustCompile(`^(no|n|nope|reject|rejected|decline|incorrect)(\b|[.!,:])`)
	if yesPattern.MatchString(normalized) {
		return DecisionYes
	}
	if noPattern.MatchString(normalized) {
		return DecisionNo
	}
	return DecisionUnknown
}

func findDeterministicContradiction(a, b []ClaimRecord) (string, string) {
	for _, claimType := range []string{"amount", "date"} {
		aClaims := claimsOfType(a, claimType)
		bClaims := claimsOfType(b, claimType)
		if len(aClaims) == 0 || len(bClaims) == 0 || claimSetsEqual(aClaims, bClaims, claimType) {
			continue
		}
		aVague, bVague := containsVague(aClaims), containsVague(bClaims)
		if aVague && !bVague {
			return "A", claimType
		}
		if bVague && !aVague {
			return "B", claimType
		}
		// Stable tie-break: ask B first. Subsequent corrections naturally change
		// the stored set; the per-party bound prevents an infinite loop.
		return "B", claimType
	}
	return "", ""
}

func claimsOfType(claims []ClaimRecord, claimType string) []ClaimRecord {
	var result []ClaimRecord
	for _, claim := range claims {
		if claim.ClaimType == claimType {
			result = append(result, claim)
		}
	}
	return result
}

func containsVague(claims []ClaimRecord) bool {
	for _, claim := range claims {
		if claim.Confidence == "vague" {
			return true
		}
	}
	return false
}

func claimSetsEqual(a, b []ClaimRecord, claimType string) bool {
	values := func(claims []ClaimRecord) map[string]bool {
		result := make(map[string]bool)
		for _, claim := range claims {
			result[canonicalClaimValue(claim.ValueText, claimType)] = true
		}
		return result
	}
	aValues, bValues := values(a), values(b)
	if len(aValues) != len(bValues) {
		return false
	}
	for value := range aValues {
		if !bValues[value] {
			return false
		}
	}
	return true
}

func canonicalClaimValue(value, claimType string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if claimType == "amount" {
		cleaned := regexp.MustCompile(`[^0-9.\-]`).ReplaceAllString(value, "")
		if number, err := strconv.ParseFloat(cleaned, 64); err == nil {
			return strconv.FormatFloat(number, 'f', 2, 64)
		}
	}
	return strings.Join(strings.Fields(value), " ")
}

func getPartyDetailsByRole(ctx context.Context, tx pgx.Tx, caseID, role string) (partyID, convID, channel string, err error) {
	err = tx.QueryRow(ctx, `
		SELECT p.id, cl.conversation_id, cl.channel 
		FROM parties p
		JOIN conversation_links cl ON p.id = cl.party_id
		WHERE p.case_id = $1 AND p.role = $2`,
		caseID, role).Scan(&partyID, &convID, &channel)
	return
}

func sendProactiveMessage(ctx context.Context, conversationID, text string) error {
	url := "http://localhost:8081/send"
	payload := map[string]string{
		"conversation_id": conversationID,
		"text":            text,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := os.Getenv("ADAPTER_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("proactive send returned status code %d", resp.StatusCode)
	}
	return nil
}

func enqueueProactiveMessage(ctx context.Context, tx pgx.Tx, caseID, partyID, conversationID, channel, messageText string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO outbound_messages (id, case_id, party_id, conversation_id, channel, text)
		VALUES ($1, $2, $3, $4, $5, $6)`, generateUUID(), caseID, partyID, conversationID, channel, messageText)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO messages_log (id, case_id, party_id, direction, channel, raw_text)
		VALUES ($1, $2, $3, 'out', $4, $5)`, generateUUID(), caseID, partyID, channel, messageText)
	return err
}

func runOutboxWorker(ctx context.Context, pool *pgxpool.Pool) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := deliverOneOutboxMessage(ctx, pool); err != nil {
				log.Printf("Outbox delivery error: %v", err)
			}
		}
	}
}

func runInactivityWorker(ctx context.Context, pool *pgxpool.Pool, timeout time.Duration) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := stallOneInactiveCase(ctx, pool, timeout); err != nil {
				log.Printf("Inactivity worker error: %v", err)
			}
		}
	}
}

func stallOneInactiveCase(ctx context.Context, pool *pgxpool.Pool, timeout time.Duration) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var caseID string
	err = tx.QueryRow(ctx, `SELECT id FROM cases
		WHERE status NOT IN ('RESOLVED','STALLED') AND updated_at < NOW() - $1::interval
		ORDER BY updated_at FOR UPDATE SKIP LOCKED LIMIT 1`, timeout.String()).Scan(&caseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE cases SET status = 'STALLED', updated_at = NOW() WHERE id = $1`, caseID); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT p.id, cl.conversation_id, cl.channel FROM parties p
		JOIN conversation_links cl ON cl.party_id = p.id WHERE p.case_id = $1`, caseID)
	if err != nil {
		return err
	}
	type destination struct{ partyID, conversationID, channel string }
	var destinations []destination
	for rows.Next() {
		var d destination
		if err := rows.Scan(&d.partyID, &d.conversationID, &d.channel); err != nil {
			rows.Close()
			return err
		}
		destinations = append(destinations, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, d := range destinations {
		msg := "Mediation has stalled because the case was inactive beyond the response deadline."
		if err := enqueueProactiveMessage(ctx, tx, caseID, d.partyID, d.conversationID, d.channel, msg); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func deliverOneOutboxMessage(ctx context.Context, pool *pgxpool.Pool) error {
	var id, caseID, conversationID, messageText string
	err := pool.QueryRow(ctx, `
		UPDATE outbound_messages SET status = 'processing', attempts = attempts + 1
		WHERE id = (SELECT id FROM outbound_messages WHERE status IN ('pending','failed') AND attempts < 5 ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING id, case_id, conversation_id, text`).Scan(&id, &caseID, &conversationID, &messageText)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := sendProactiveMessage(ctx, conversationID, messageText); err != nil {
		_, updateErr := pool.Exec(ctx, `UPDATE outbound_messages SET status = 'failed', last_error = $2 WHERE id = $1`, id, err.Error())
		if updateErr != nil {
			return updateErr
		}
		if _, issueErr := pool.Exec(ctx, `UPDATE cases SET delivery_issue = CASE WHEN (SELECT attempts FROM outbound_messages WHERE id = $2) >= 5 THEN $3 ELSE delivery_issue END WHERE id = $1`, caseID, id, "Proactive message delivery failed after five attempts"); issueErr != nil {
			return issueErr
		}
		return err
	}
	_, err = pool.Exec(ctx, `UPDATE outbound_messages SET status = 'sent', sent_at = NOW(), last_error = NULL WHERE id = $1`, id)
	return err
}

func ensureRuntimeSchema(ctx context.Context, pool *pgxpool.Pool) error {
	statements := []string{
		`ALTER TABLE cases ADD COLUMN IF NOT EXISTS cross_check_rounds_a SMALLINT NOT NULL DEFAULT 0`,
		`ALTER TABLE cases ADD COLUMN IF NOT EXISTS cross_check_rounds_b SMALLINT NOT NULL DEFAULT 0`,
		`ALTER TABLE cases ADD COLUMN IF NOT EXISTS delivery_issue TEXT`,
		`ALTER TABLE consents ADD COLUMN IF NOT EXISTS objection_claim_ids JSONB NOT NULL DEFAULT '[]'`,
		`CREATE TABLE IF NOT EXISTS outbound_messages (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(), case_id UUID NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
			party_id UUID NOT NULL REFERENCES parties(id) ON DELETE CASCADE, conversation_id VARCHAR(255) NOT NULL,
			channel VARCHAR(20) NOT NULL, text TEXT NOT NULL, status VARCHAR(12) NOT NULL DEFAULT 'pending'
			CHECK (status IN ('pending','processing','sent','failed')), attempts SMALLINT NOT NULL DEFAULT 0,
			last_error TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), sent_at TIMESTAMPTZ)`,
		`CREATE INDEX IF NOT EXISTS idx_outbound_messages_pending ON outbound_messages (status, created_at)`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			return err
		}
	}
	// A process may have stopped after claiming an outbox row but before
	// delivery. Requeue those rows on startup; downstream retries are bounded.
	_, err := pool.Exec(ctx, `UPDATE outbound_messages SET status = 'failed', last_error = 'worker restarted during delivery' WHERE status = 'processing'`)
	if err != nil {
		return err
	}
	return nil
}

// --- Utils ---

func generateUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4
	b[8] = (b[8] & 0x3f) | 0x80 // Variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func generateCode(length int) string {
	const charset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, length)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		b[i] = charset[n.Int64()]
	}
	return string(b)
}

func isAlphanumericUpper(s string) bool {
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
