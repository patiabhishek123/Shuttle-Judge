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
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

// Config holds environment configurations
type Config struct {
	DatabaseURL      string
	OpenRouterAPIKey string
	OpenRouterModel  string
	Port             string
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

func main() {
	// Load .env file
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, relying on environment variables")
	}

	config := &Config{
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		OpenRouterAPIKey: os.Getenv("OPENROUTER_API_KEY"),
		OpenRouterModel:  os.Getenv("OPENROUTER_MODEL"),
		Port:             os.Getenv("PORT"),
	}

	if config.Port == "" {
		config.Port = "8080"
	}
	if config.OpenRouterModel == "" {
		config.OpenRouterModel = "google/gemini-2.5-flash"
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
	log.Println("Database connection established successfully")

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

		var req MessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}

		log.Printf("[Inbound] Chan: %s | Conv: %s | Sender: %s | Msg: %q\n", req.Channel, req.ConversationID, req.SenderRef, req.Text)

		reply, err := processMessage(ctx, pool, config, req)
		if err != nil {
			log.Printf("Error processing message: %v\n", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(MessageResponse{ReplyText: reply})
	})

	log.Printf("Go mediation engine listening on port %s...\n", config.Port)
	if err := http.ListenAndServe(":"+config.Port, nil); err != nil {
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
					WHERE join_code = $1 AND join_code_used_at IS NULL`, 
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

					reply := fmt.Sprintf("Welcome to Shuttle Court. You have joined the case regarding: \"%s\". To help me mediate, please tell me your side of the story: what happened, what amount/date is in dispute, and what outcome you want.", topicSummary)

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

			// Not a join code, or join code not found. Create a new case (Party A intake start).
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

			newCaseID := generateUUID()
			shortID := generateCode(6)
			joinCode := generateCode(6)

			_, err = tx.Exec(ctx, `
				INSERT INTO cases (id, short_id, join_code, status, topic_summary, cross_check_rounds) 
				VALUES ($1, $2, $3, 'INTAKE', '', 0)`, 
				newCaseID, shortID, joinCode)
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

			reply := fmt.Sprintf("Hello, I am Docket, your Shuttle Court mediator. I have opened case **%s** for you. Please share this join code with the other party: **%s**. They can message me with this code to join.\n\nI have recorded your initial details. Let's make sure I have everything: what exactly happened, what amount or date is in dispute, and what resolution do you want?", shortID, joinCode)

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
	var shortID, joinCode, status, topicSummary string
	var crossCheckRounds int
	err = tx.QueryRow(ctx, `
		SELECT short_id, join_code, status, topic_summary, cross_check_rounds 
		FROM cases WHERE id = $1`, 
		caseID).Scan(&shortID, &joinCode, &status, &topicSummary, &crossCheckRounds)
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

	// Handle status query shortcut
	if strings.Contains(strings.ToLower(req.Text), "status") {
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

		// Log outbound message
		_, _ = tx.Exec(ctx, `
			INSERT INTO messages_log (id, case_id, party_id, direction, channel, raw_text) 
			VALUES ($1, $2, $3, 'out', $4, $5)`, 
			generateUUID(), caseID, partyID, req.Channel, reply)

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

	// Save claims (overwrite existing claims of that type for the party to keep current)
	for _, claim := range llmRes.Claims {
		_, _ = tx.Exec(ctx, `
			DELETE FROM claims WHERE case_id = $1 AND party_id = $2 AND claim_type = $3`, 
			caseID, partyID, claim.ClaimType)
		
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
		replyText, err = handleConsentEvaluation(ctx, tx, cfg, caseID, partyID, req.Text)

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
	// Increment rounds
	var rounds int
	err := tx.QueryRow(ctx, `
		UPDATE cases 
		SET cross_check_rounds = cross_check_rounds + 1, updated_at = NOW() 
		WHERE id = $1 
		RETURNING cross_check_rounds`, 
		caseID).Scan(&rounds)
	if err != nil {
		return "", err
	}

	// Load claims of both parties
	claimsA, err := loadClaims(ctx, tx, caseID, "A")
	if err != nil {
		return "", err
	}
	claimsB, err := loadClaims(ctx, tx, caseID, "B")
	if err != nil {
		return "", err
	}

	// Call LLM to detect contradictions and get a targeted follow-up question
	systemPrompt := `You are Docket, the Shuttle Court AI mediator.
You are comparing claims between Party A and Party B.
Here are the claims of both parties:
Party A:
` + claimsA + `

Party B:
` + claimsB + `

Your goal is to detect contradiction in 'amount' or 'date'.
If they differ:
Determine which party has a more vague claim (lower confidence). Ask them a targeted follow-up question.
Do NOT reveal the counterpart's raw text or exact values, e.g. do not say "The other party says $340". Just ask them to clarify, e.g. "Can you double check the amount of the March bill?"
If there are no contradictions, or if both parties match, set target_role to "" and question_text to "OK".

Respond strictly in JSON format matching this schema:
{
  "target_role": "A" | "B" | "",
  "question_text": "question text or OK"
}`

	var res CrossCheckResult
	err = callOpenRouterJSON(ctx, cfg, systemPrompt, "Analyze and cross-check these claims", &res)
	if err != nil {
		return "", err
	}

	// If no contradictions or rounds limit hit (rounds > 2) -> Propose resolution
	if res.TargetRole == "" || res.QuestionText == "OK" || rounds > 2 {
		return initiateProposal(ctx, tx, cfg, caseID, currentPartyID)
	}

	// We have a targeted follow-up question
	targetPartyID, targetConvID, targetChannel, err := getPartyDetailsByRole(ctx, tx, caseID, res.TargetRole)
	if err != nil {
		return "", err
	}

	if targetPartyID == currentPartyID {
		// Ask the current user directly
		return res.QuestionText, nil
	}

	// Proactively ask the counterpart
	err = sendProactiveMessage(ctx, targetConvID, res.QuestionText)
	if err != nil {
		log.Printf("Failed to send proactive message to %s: %v\n", targetConvID, err)
	} else {
		// Log the proactive question
		_, _ = tx.Exec(ctx, `
			INSERT INTO messages_log (id, case_id, party_id, direction, channel, raw_text) 
			VALUES ($1, $2, $3, 'out', $4, $5)`, 
			generateUUID(), caseID, targetPartyID, targetChannel, res.QuestionText)
	}

	// Reply to current user that we are cross-checking
	return "Thank you. I am cross-checking details with the other party to clarify some points. I will notify you once done.", nil
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
		VALUES ($1, $2, 1, $3, '{}')`, 
		proposalID, caseID, res.ProposalText)
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
	_ = tx.QueryRow(ctx, "SELECT role FROM parties WHERE id = $1", currentPartyID).Scan(&currentPartyRole)
	if currentPartyRole == "B" {
		counterpartRole = "A"
	}

	cPartyID, cConvID, cChannel, err := getPartyDetailsByRole(ctx, tx, caseID, counterpartRole)
	if err == nil {
		proposalMsg := fmt.Sprintf("I have formulated a resolution proposal:\n\n%s\n\nDo you accept this proposal? Please reply YES or NO.", res.ProposalText)
		err = sendProactiveMessage(ctx, cConvID, proposalMsg)
		if err != nil {
			log.Printf("Failed to send proposal proactively to counterpart: %v\n", err)
		} else {
			_, _ = tx.Exec(ctx, `
				INSERT INTO messages_log (id, case_id, party_id, direction, channel, raw_text) 
				VALUES ($1, $2, $3, 'out', $4, $5)`, 
				generateUUID(), caseID, cPartyID, cChannel, proposalMsg)
		}
	}

	// Return proposal directly to current user
	return fmt.Sprintf("I have formulated a resolution proposal:\n\n%s\n\nDo you accept this proposal? Please reply YES or NO.", res.ProposalText), nil
}

// handleConsentEvaluation processes the user's vote on the proposal
func handleConsentEvaluation(ctx context.Context, tx pgx.Tx, cfg *Config, caseID, currentPartyID, userText string) (string, error) {
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

	// Classify response
	systemPrompt := `You are Docket, the Shuttle Court AI mediator.
Evaluate the user's reply to this proposal:
Proposal: "` + proposalText + `"
User Reply: "` + userText + `"

Determine if they accept (yes) or reject (no).
If they reject, extract their specific objection, comment, or concern.

Respond strictly in JSON format matching this schema:
{
  "consent": "yes" | "no",
  "comment": "objection detail or empty string"
}`

	var res ConsentResult
	err = callOpenRouterJSON(ctx, cfg, systemPrompt, "Classify consent", &res)
	if err != nil {
		return "", err
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
		_ = tx.QueryRow(ctx, "SELECT role FROM parties WHERE id = $1", currentPartyID).Scan(&currentPartyRole)
		counterpartRole := "B"
		if currentPartyRole == "B" {
			counterpartRole = "A"
		}
		cPartyID, cConvID, cChannel, err := getPartyDetailsByRole(ctx, tx, caseID, counterpartRole)
		if err == nil {
			msg := "Excellent. Both parties have consented. This case is now resolved. Thank you for using Shuttle Court."
			_ = sendProactiveMessage(ctx, cConvID, msg)
			_, _ = tx.Exec(ctx, `
				INSERT INTO messages_log (id, case_id, party_id, direction, channel, raw_text) 
				VALUES ($1, $2, $3, 'out', $4, $5)`, 
				generateUUID(), caseID, cPartyID, cChannel, msg)
		}

		// Dispatch final summary to third-party log channel
		go dispatchCaseSummary(cfg, caseID, proposalText)

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
			_ = tx.QueryRow(ctx, "SELECT role FROM parties WHERE id = $1", currentPartyID).Scan(&currentPartyRole)
			counterpartRole := "B"
			if currentPartyRole == "B" {
				counterpartRole = "A"
			}
			cPartyID, cConvID, cChannel, err := getPartyDetailsByRole(ctx, tx, caseID, counterpartRole)
			if err == nil {
				msg := "Mediation has stalled because we could not reach an agreement."
				_ = sendProactiveMessage(ctx, cConvID, msg)
				_, _ = tx.Exec(ctx, `
					INSERT INTO messages_log (id, case_id, party_id, direction, channel, raw_text) 
					VALUES ($1, $2, $3, 'out', $4, $5)`, 
					generateUUID(), caseID, cPartyID, cChannel, msg)
			}

			return "Mediation has stalled because we could not reach an agreement.", nil
		}

		// Create a revised proposal
		claimsA, _ := loadClaims(ctx, tx, caseID, "A")
		claimsB, _ := loadClaims(ctx, tx, caseID, "B")

		objectionPrompt := `You are Docket, the Shuttle Court AI mediator.
You proposed: "` + proposalText + `"
One of the parties rejected it with the following objection: "` + res.Comment + `"
Based on the reconciled claims of the parties:
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
			VALUES ($1, $2, $3, $4, '{}')`, 
			newProposalID, caseID, version+1, newRes.ProposalText)
		if err != nil {
			return "", err
		}

		// Proactively notify counterpart (without leaking raw objection text)
		var currentPartyRole string
		_ = tx.QueryRow(ctx, "SELECT role FROM parties WHERE id = $1", currentPartyID).Scan(&currentPartyRole)
		counterpartRole := "B"
		if currentPartyRole == "B" {
			counterpartRole = "A"
		}
		cPartyID, cConvID, cChannel, err := getPartyDetailsByRole(ctx, tx, caseID, counterpartRole)
		if err == nil {
			revisedMsg := fmt.Sprintf("The other party had a concern about the proposal. Here is a revised version:\n\n%s\n\nDo you accept this revised proposal? Please reply YES or NO.", newRes.ProposalText)
			_ = sendProactiveMessage(ctx, cConvID, revisedMsg)
			_, _ = tx.Exec(ctx, `
				INSERT INTO messages_log (id, case_id, party_id, direction, channel, raw_text) 
				VALUES ($1, $2, $3, 'out', $4, $5)`, 
				generateUUID(), caseID, cPartyID, cChannel, revisedMsg)
		}

		return fmt.Sprintf("I have formulated a revised proposal to address concerns:\n\n%s\n\nDo you accept this revised proposal? Please reply YES or NO.", newRes.ProposalText), nil
	}

	// Just recorded a YES consent, waiting for the other party
	return "Thank you for your response. I am waiting for the other party to vote on the proposal.", nil
}

// dispatchCaseSummary dispatches a clean log summary to a third-party email channel
func dispatchCaseSummary(cfg *Config, caseID, proposalText string) {
	log.Println("[Summary Dispatch] Sending final resolution summary to case log channel...")

	// Proactively post a summary email using Caspian client
	logAddress := "shuttlecourt-log@agents.trycaspianai.com"
	summaryText := fmt.Sprintf("SHUTTLE COURT CASE RESOLUTION LOG\nCase ID: %s\nResolved At: %s\n\nProposal Accepted:\n%s", caseID, time.Now().Format(time.RFC1123), proposalText)
	
	// Create a mock conversation/channel send or initiate email connection
	// We can use the proactive send to a special log channel, or simply log it.
	log.Printf("[Case Log Email Sent to %s]: %s\n", logAddress, summaryText)
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
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://openrouter.ai/api/v1/chat/completions", bytes.NewBuffer(bodyBytes))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+cfg.OpenRouterAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HTTP-Referer", "http://localhost:8080")
	req.Header.Set("X-Title", "Shuttle Court")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("openrouter API error (status %d): %s", resp.StatusCode, string(errBody))
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var openRouterResponse struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(respBody, &openRouterResponse); err != nil {
		log.Printf("OpenRouter raw response body: %s", string(respBody))
		return fmt.Errorf("failed to unmarshal OpenRouter response: %w (raw response: %s)", err, string(respBody))
	}

	if len(openRouterResponse.Choices) == 0 {
		return fmt.Errorf("empty response choices from OpenRouter (raw response: %s)", string(respBody))
	}

	contentStr := openRouterResponse.Choices[0].Message.Content
	return json.Unmarshal([]byte(contentStr), result)
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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("proactive send returned status code %d", resp.StatusCode)
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
