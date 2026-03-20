package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/stripe/stripe-go/v84"
	"github.com/stripe/stripe-go/v84/paymentintent"
)

func (app *App) CreatePaymentIntent(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// 1. Read the cart cookie just like we do in the Cart/Checkout view
	type CartCookieItem struct {
		ProductID string `json:"productId"`
		Quantity  int    `json:"quantity"`
	}

	var cartItems []CartCookieItem
	if cookie, err := r.Cookie("cart"); err == nil {
		decoded, _ := url.QueryUnescape(cookie.Value)
		_ = json.Unmarshal([]byte(decoded), &cartItems)
	}

	if len(cartItems) == 0 {
		http.Error(w, "Cart is empty", http.StatusBadRequest)
		return
	}

	var ids []int64
	quantityMap := make(map[int64]int)
	for _, item := range cartItems {
		if id, err := strconv.ParseInt(item.ProductID, 10, 64); err == nil {
			ids = append(ids, id)
			quantityMap[id] = item.Quantity
		}
	}

	// 2. Query the database to calculate the REAL total
	var subtotal float64
	rows, err := app.DB.Query(ctx, "SELECT id, price, discount_price FROM products WHERE id = ANY($1) AND is_active = true", ids)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id int64
			var price float64
			var discount *float64
			if err := rows.Scan(&id, &price, &discount); err == nil {
				activePrice := price
				if discount != nil {
					activePrice = *discount
				}
				subtotal += activePrice * float64(quantityMap[id])
			}
		}
	}

	// Calculate Tax and Shipping (Hardcoded for now to match your frontend logic)
	tax := subtotal * 0.08
	shipping := 0.00 // Assuming free for this example
	totalAmount := subtotal + tax + shipping

	// Stripe requires amounts in cents (e.g., $10.50 = 1050)
	amountInCents := int64(totalAmount * 100)

	// 3. Create the Stripe Payment Intent
	params := &stripe.PaymentIntentParams{
		Amount:   new(amountInCents),
		Currency: stripe.String(string(stripe.CurrencyUSD)),
		AutomaticPaymentMethods: &stripe.PaymentIntentAutomaticPaymentMethodsParams{
			Enabled: new(true),
		},
	}

	pi, err := paymentintent.New(params)
	if err != nil {
		app.Logger.Error("Stripe payment intent failed", "error", err)
		http.Error(w, "Payment processing error", http.StatusInternalServerError)
		return
	}

	// 4. Send the secure client_secret to the frontend
	app.WriteJSON(w, r, http.StatusOK, map[string]string{
		"clientSecret": pi.ClientSecret,
	})
}

type CreateOrderRequest struct {
	PaymentIntentID string `json:"paymentIntentId"`
	Name            string `json:"name"`
}

func (app *App) CreateOrder(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var req CreateOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// 1. Get User ID & Email (if logged in). Fallback for guests.
	var userID *int64
	email := "guest@example.com"

	if uid, err := app.validateUserSession(r, ctx); err == nil {
		userID = &uid
		app.DB.QueryRow(ctx, "SELECT email FROM users WHERE id = $1", uid).Scan(&email)
	}

	// 2. Read the Cart Cookie
	type CartCookieItem struct {
		ProductID string `json:"productId"`
		Quantity  int    `json:"quantity"`
	}
	var cartItems []CartCookieItem
	if cookie, err := r.Cookie("cart"); err == nil {
		decoded, _ := url.QueryUnescape(cookie.Value)
		_ = json.Unmarshal([]byte(decoded), &cartItems)
	}

	if len(cartItems) == 0 {
		http.Error(w, "Cart is empty", http.StatusBadRequest)
		return
	}

	// 3. Calculate Total and Prepare Items
	var totalAmount float64
	type DbItem struct {
		ProductID int64
		Price     float64
		Qty       int
	}
	var itemsToSave []DbItem

	for _, item := range cartItems {
		pid, _ := strconv.ParseInt(item.ProductID, 10, 64)
		var price float64
		var discount *float64

		err := app.DB.QueryRow(ctx, "SELECT price, discount_price FROM products WHERE id = $1", pid).Scan(&price, &discount)
		if err == nil {
			activePrice := price
			if discount != nil {
				activePrice = *discount
			}
			totalAmount += activePrice * float64(item.Quantity)
			itemsToSave = append(itemsToSave, DbItem{ProductID: pid, Price: activePrice, Qty: item.Quantity})
		}
	}

	totalAmount = totalAmount + (totalAmount * 0.08) // Add 8% tax to match frontend

	// 4. Generate Order ID (e.g., ORD-84729)
	orderID := fmt.Sprintf("ORD-%d", time.Now().Unix()%100000)

	// 5. Insert Order
	_, err := app.DB.Exec(ctx, `
		INSERT INTO orders (id, user_id, customer_name, customer_email, total_amount, status) 
		VALUES ($1, $2, $3, $4, $5, 'Processing')
	`, orderID, userID, req.Name, email, totalAmount)

	if err != nil {
		app.Logger.Error("Failed to save order", "error", err)
		http.Error(w, "Failed to save order", http.StatusInternalServerError)
		return
	}

	// 6. Insert Order Items
	for _, item := range itemsToSave {
		app.DB.Exec(ctx, "INSERT INTO order_items (order_id, product_id, quantity, price) VALUES ($1, $2, $3, $4)",
			orderID, item.ProductID, item.Qty, item.Price)
	}

	// 7. Insert Payment Record
	amountInCents := int(totalAmount * 100)
	_, err = app.DB.Exec(ctx, `
		INSERT INTO payments (order_id, stripe_payment_intent_id, amount, status) 
		VALUES ($1, $2, $3, 'succeeded')
	`, orderID, req.PaymentIntentID, amountInCents)

	if err != nil {
		app.Logger.Error("Failed to save payment record", "error", err)
	}

	// 8. Success!
	app.WriteJSON(w, r, http.StatusOK, map[string]string{"orderId": orderID})
}
