package routes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type App struct {
	DB         *pgxpool.Pool
	Templates  embed.FS
	Logger     *slog.Logger
	AppVersion string

	templateCache map[string]*template.Template
	cacheMutex    sync.RWMutex
}

// --- Public Routes ---
type ProductCombo struct {
	ID          int64
	ComboString string
}

type HomeGroup struct {
	ID        int64
	Name      string
	MainImage string
	MinPrice  float64
	Combos    []ProductCombo
}

type HomeDataResponse struct {
	Categories []Category
	Groups     []HomeGroup
}

func (app *App) Home(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// 1. Fetch top 12 product groups and all their active products
	query := `
		WITH top_groups AS (
			SELECT id, name FROM product_groups ORDER BY created_at DESC LIMIT 12
		)
		SELECT g.id, g.name, p.id, p.price, p.discount_price, p.gallery, p.variations
		FROM top_groups g
		JOIN products p ON g.id = p.group_id
		WHERE p.is_active = true
		ORDER BY g.id DESC, p.created_at ASC
	`

	rows, err := app.DB.Query(ctx, query)
	if err != nil {
		app.Logger.Error("Failed to fetch grouped products for home page", "error", err)
	}

	groupsMap := make(map[int64]*HomeGroup)
	var orderedGroups []*HomeGroup

	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var gID, pID int64
			var gName string
			var pPrice float64
			var pDisc *float64
			var pGal, pVars []byte

			if err := rows.Scan(&gID, &gName, &pID, &pPrice, &pDisc, &pGal, &pVars); err != nil {
				continue
			}

			// Determine lowest price
			activePrice := pPrice
			if pDisc != nil {
				activePrice = *pDisc
			}

			// Extract primary image
			var gallery []string
			_ = json.Unmarshal(pGal, &gallery)
			img := ""
			if len(gallery) > 0 {
				img = gallery[0]
			}

			// Extract and consistently sort variations to build the "Red / Large" string
			var vars map[string]string
			_ = json.Unmarshal(pVars, &vars)

			var keys []string
			for k := range vars {
				keys = append(keys, k)
			}
			sort.Strings(keys) // Ensures "Color" always comes before "Size" alphabetically

			var comboParts []string
			for _, k := range keys {
				comboParts = append(comboParts, vars[k])
			}
			comboStr := strings.Join(comboParts, " / ")
			if comboStr == "" {
				comboStr = "Standard"
			}

			// Group aggregation logic
			if group, exists := groupsMap[gID]; exists {
				group.Combos = append(group.Combos, ProductCombo{ID: pID, ComboString: comboStr})
				if activePrice < group.MinPrice {
					group.MinPrice = activePrice
				}
				if group.MainImage == "" && img != "" {
					group.MainImage = img
				}
			} else {
				newGroup := &HomeGroup{
					ID:        gID,
					Name:      gName,
					MainImage: img,
					MinPrice:  activePrice,
					Combos:    []ProductCombo{{ID: pID, ComboString: comboStr}},
				}
				groupsMap[gID] = newGroup
				orderedGroups = append(orderedGroups, newGroup)
			}
		}
	}

	var finalGroups []HomeGroup
	for _, g := range orderedGroups {
		finalGroups = append(finalGroups, *g)
	}
	if finalGroups == nil {
		finalGroups = []HomeGroup{}
	}

	data := HomeDataResponse{
		Categories: []Category{
			{1, "Electronics", "electronics", "laptop"},
			{2, "Fashion", "fashion", "shirt"},
			{3, "Home & Garden", "home", "house"},
			{4, "Sports", "sports", "dumbbell"},
			{5, "Toys", "toys", "gamepad-2"},
			{6, "Books", "books", "book"},
		},
		Groups: finalGroups,
	}

	app.renderPage(w, r, data, "templates/index.html")
}

func (app *App) Search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	cat := r.URL.Query().Get("category")

	d2799, d450, d150, d39 := 2799, 450, 150, 39
	library := []SearchProduct{
		{1, "Apple iMac 27\"", "Electronics", 2999, &d2799, 5.0, 120, "Fri, Jan 16"},
		{2, "Playstation 5 DualSense", "Gaming", 499, nil, 4.8, 85, "Sat, Jan 17"},
		{3, "Xbox Series X Console", "Gaming", 499, &d450, 4.9, 210, "Fri, Jan 16"},
		{4, "Macbook Pro 14 M3", "Electronics", 1999, nil, 5.0, 45, "Fri, Jan 16"},
		{5, "Leather Bomber Jacket", "Fashion", 199, &d150, 4.5, 32, "Mon, Jan 19"},
		{6, "Smart Coffee Maker", "Home", 129, nil, 4.2, 18, "Fri, Jan 16"},
		{7, "4K Gaming Monitor", "Electronics", 599, &d39, 4.7, 64, "Sun, Jan 18"},
		{8, "Wireless Mechanical Keyboard", "Electronics", 149, nil, 4.6, 92, "Fri, Jan 16"},
		{9, "Lego Star Wars Set", "Toys", 89, nil, 4.9, 200, "Tue, Jan 20"},
		{10, "RC Drift Car", "Toys", 45, &d39, 4.3, 56, "Wed, Jan 21"},
	}

	var results []SearchProduct
	for _, p := range library {
		if (q == "" || contains(p.Name, q)) &&
			(cat == "" || contains(p.Category, cat)) {
			results = append(results, p)
		}
	}

	pageData := struct {
		Products []SearchProduct
	}{
		Products: results,
	}

	app.renderPage(w, r, pageData, "templates/search.html")
}

func (app *App) ProductDetail(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	productIDStr := r.PathValue("productID")
	productID, err := strconv.ParseInt(productIDStr, 10, 64)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	var data ProductDetailResponse
	var groupID int64
	var galleryJSON, varsJSON, vTypesJSON []byte
	var currentVars map[string]string

	// 1. FETCH PRODUCT & GROUP INFO
	err = app.DB.QueryRow(ctx, `
		SELECT p.id, p.group_id, p.name, p.description, p.price, p.discount_price, p.gallery, p.variations, g.variation_types
		FROM products p
		JOIN product_groups g ON p.group_id = g.id
		WHERE p.id = $1 AND p.is_active = true
	`, productID).Scan(&data.ID, &groupID, &data.Name, &data.Description, &data.Price, &data.DiscountPrice, &galleryJSON, &varsJSON, &vTypesJSON)

	if err != nil {
		app.Logger.Error("Product not found", "error", err)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	_ = json.Unmarshal(galleryJSON, &data.Gallery)
	_ = json.Unmarshal(varsJSON, &currentVars)

	// Pass current vars to frontend as string
	cvBytes, _ := json.Marshal(currentVars)
	data.CurrentVarsJSON = string(cvBytes)

	var variationTypes []string
	_ = json.Unmarshal(vTypesJSON, &variationTypes)

	// 2. FETCH SIBLING PRODUCTS (To figure out available variation options)
	type Sibling struct {
		ID         int64             `json:"id"`
		Variations map[string]string `json:"variations"`
	}
	var siblings []Sibling
	uniqueOptions := make(map[string]map[string]bool)
	for _, vt := range variationTypes {
		uniqueOptions[vt] = make(map[string]bool)
	}

	sibRows, _ := app.DB.Query(ctx, "SELECT id, variations FROM products WHERE group_id = $1 AND is_active = true", groupID)
	defer sibRows.Close()

	for sibRows.Next() {
		var sib Sibling
		var sVarsJSON []byte
		if err := sibRows.Scan(&sib.ID, &sVarsJSON); err == nil {
			_ = json.Unmarshal(sVarsJSON, &sib.Variations)
			siblings = append(siblings, sib)

			// Map available options (e.g. Color: Red, Blue)
			for k, v := range sib.Variations {
				if _, exists := uniqueOptions[k]; exists {
					uniqueOptions[k][v] = true
				}
			}
		}
	}

	// Pass siblings to frontend for routing
	sibBytes, _ := json.Marshal(siblings)
	data.SiblingsJSON = string(sibBytes)

	// Build the Variations array for the UI
	for _, vt := range variationTypes {
		var opts []string
		for opt := range uniqueOptions[vt] {
			opts = append(opts, opt)
		}
		data.Variations = append(data.Variations, VariationGroup{Name: vt, Options: opts})
	}

	// 3. FETCH REVIEWS & CALCULATE STATS
	statsMap := map[int]int{1: 0, 2: 0, 3: 0, 4: 0, 5: 0}
	totalReviews, totalStars := 0, 0

	reviewRows, _ := app.DB.Query(ctx, `
		SELECT r.rating, r.comment, COALESCE(u.name, 'Customer'), r.created_at
		FROM reviews r
		JOIN users u ON r.user_id = u.id
		WHERE r.product_id = $1
		ORDER BY r.created_at DESC
	`, productID)
	defer reviewRows.Close()

	for reviewRows.Next() {
		var rev DetailReview
		var createdAt time.Time
		if err := reviewRows.Scan(&rev.Rating, &rev.Comment, &rev.UserName, &createdAt); err == nil {
			rev.Date = createdAt.Format("Jan 02, 2006")
			rev.IsVerified = true // Assuming all reviews are verified for now

			// Generate Initials
			parts := strings.Split(rev.UserName, " ")
			if len(parts) > 1 {
				rev.Initials = strings.ToUpper(string(parts[0][0]) + string(parts[1][0]))
			} else if len(parts[0]) > 0 {
				rev.Initials = strings.ToUpper(string(parts[0][0]))
			}

			data.Reviews = append(data.Reviews, rev)

			// Tally stats
			statsMap[rev.Rating]++
			totalReviews++
			totalStars += rev.Rating
		}
	}

	data.ReviewsCount = totalReviews
	if totalReviews > 0 {
		data.Rating = float64(totalStars) / float64(totalReviews)
	}

	// Build the percentage array (5 stars down to 1)
	for i := 5; i >= 1; i-- {
		count := statsMap[i]
		pct := 0
		if totalReviews > 0 {
			pct = int((float64(count) / float64(totalReviews)) * 100)
		}
		data.ReviewStats = append(data.ReviewStats, ReviewStat{Stars: i, Count: count, Percentage: pct})
	}

	app.renderPage(w, r, data, "templates/product.html")
}

// --- Auth & Cart Pages ---

func (app *App) Login(w http.ResponseWriter, r *http.Request) {
	app.renderPage(w, r, nil, "templates/login.html")
}

func (app *App) Register(w http.ResponseWriter, r *http.Request) {
	app.renderPage(w, r, nil, "templates/register.html")
}

func (app *App) Cart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// 1. Read the Cart Cookie
	type CartCookieItem struct {
		ProductID string `json:"productId"`
		Quantity  int    `json:"quantity"`
	}

	var cartItems []CartCookieItem
	if cookie, err := r.Cookie("cart"); err == nil {
		if decoded, err := url.QueryUnescape(cookie.Value); err == nil {
			_ = json.Unmarshal([]byte(decoded), &cartItems)
		}
	}

	// 2. Extract IDs and build a Quantity map
	var ids []int64
	quantityMap := make(map[int64]int)

	for _, item := range cartItems {
		id, err := strconv.ParseInt(item.ProductID, 10, 64)
		if err == nil {
			ids = append(ids, id)
			quantityMap[id] = item.Quantity
		}
	}

	// 3. Define the View Struct
	type CartViewItem struct {
		ProductId int64
		Name      string
		Price     float64
		Image     string
		Quantity  int
	}

	var activeItems []CartViewItem
	var subtotal float64

	// 4. Query the Database for the cart items
	if len(ids) > 0 {
		query := `
			SELECT id, name, price, discount_price, gallery
			FROM products
			WHERE id = ANY($1) AND is_active = true
		`

		rows, err := app.DB.Query(ctx, query, ids)
		if err != nil {
			app.Logger.Error("Failed to fetch cart products", "error", err)
		} else {
			defer rows.Close()
			for rows.Next() {
				var id int64
				var name string
				var price float64
				var discountPrice *float64
				var galleryJSON []byte

				if err := rows.Scan(&id, &name, &price, &discountPrice, &galleryJSON); err == nil {
					// Use discount price if available
					activePrice := price
					if discountPrice != nil {
						activePrice = *discountPrice
					}

					// Extract first image from gallery
					var gallery []string
					_ = json.Unmarshal(galleryJSON, &gallery)
					img := "https://placehold.co/100x100?text=No+Image"
					if len(gallery) > 0 && gallery[0] != "" {
						img = gallery[0]
					}

					qty := quantityMap[id]

					activeItems = append(activeItems, CartViewItem{
						ProductId: id,
						Name:      name,
						Price:     activePrice,
						Image:     img,
						Quantity:  qty,
					})

					subtotal += activePrice * float64(qty)
				}
			}
		}
	}

	// Ensure slice is never nil for template loop
	if activeItems == nil {
		activeItems = []CartViewItem{}
	}

	pageData := struct {
		Items     []CartViewItem
		ItemCount int
		Subtotal  string
		Total     string
	}{
		Items:     activeItems,
		ItemCount: len(activeItems), // Note: This counts unique items. Use a loop sum if you want total quantity.
		Subtotal:  fmt.Sprintf("$%.2f", subtotal),
	}

	app.renderPage(w, r, pageData, "templates/cart.html")
}

func (app *App) Checkout(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// 1. Read the Cart Cookie (Works for both Guests and Logged-in Users)
	type CartCookieItem struct {
		ProductID string `json:"productId"`
		Quantity  int    `json:"quantity"`
	}

	var cartItems []CartCookieItem
	if cookie, err := r.Cookie("cart"); err == nil {
		if decoded, err := url.QueryUnescape(cookie.Value); err == nil {
			_ = json.Unmarshal([]byte(decoded), &cartItems)
		}
	}

	var ids []int64
	quantityMap := make(map[int64]int)

	for _, item := range cartItems {
		id, err := strconv.ParseInt(item.ProductID, 10, 64)
		if err == nil {
			ids = append(ids, id)
			quantityMap[id] = item.Quantity
		}
	}

	// 2. Define the View Structs
	type ShippingOption struct {
		Id    string
		Name  string
		Price float64
		Desc  string
	}

	type CheckoutViewItem struct {
		ProductId       int64
		Name            string
		Price           float64
		Quantity        int
		ItemTotal       float64
		Image           string
		Selections      map[string]string
		ShippingOptions []ShippingOption
	}

	type Address struct {
		Id      int64
		Name    string
		Street  string
		City    string
		Zip     string
		Default bool
	}

	var activeItems []CheckoutViewItem

	// 3. Query Real Product Data
	if len(ids) > 0 {
		query := `
			SELECT id, name, price, discount_price, gallery, variations, is_free_shipping
			FROM products
			WHERE id = ANY($1) AND is_active = true
		`

		rows, err := app.DB.Query(ctx, query, ids)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id int64
				var name string
				var price float64
				var discountPrice *float64
				var galleryJSON, variationsJSON []byte
				var isFreeShipping bool

				if err := rows.Scan(&id, &name, &price, &discountPrice, &galleryJSON, &variationsJSON, &isFreeShipping); err == nil {
					activePrice := price
					if discountPrice != nil {
						activePrice = *discountPrice
					}

					var gallery []string
					_ = json.Unmarshal(galleryJSON, &gallery)
					img := "https://placehold.co/100x100?text=No+Image"
					if len(gallery) > 0 && gallery[0] != "" {
						img = gallery[0]
					}

					var variations map[string]string
					_ = json.Unmarshal(variationsJSON, &variations)

					qty := quantityMap[id]

					// Generate Dynamic Shipping Options
					shippingOpts := []ShippingOption{}
					if isFreeShipping {
						shippingOpts = append(shippingOpts, ShippingOption{"free", "Free Shipping", 0.00, "5-7 Business Days"})
						shippingOpts = append(shippingOpts, ShippingOption{"express", "Express Shipping", 15.00, "2 Business Days"})
					} else {
						shippingOpts = append(shippingOpts, ShippingOption{"standard", "Standard Shipping", 5.99, "5-7 Business Days"})
						shippingOpts = append(shippingOpts, ShippingOption{"express", "Express Shipping", 19.99, "1-2 Business Days"})
					}

					activeItems = append(activeItems, CheckoutViewItem{
						ProductId:       id,
						Name:            name,
						Price:           activePrice,
						Quantity:        qty,
						ItemTotal:       activePrice * float64(qty),
						Image:           img,
						Selections:      variations,
						ShippingOptions: shippingOpts,
					})
				}
			}
		} else {
			app.Logger.Error("Failed to fetch checkout products", "error", err)
		}
	}

	// 4. Fetch Addresses (ONLY if the user is logged in)
	var addresses []Address
	userID, err := app.validateUserSession(r, ctx)

	if err == nil {
		// User is logged in! Fetch their saved addresses.
		// (This query runs safely; if the addresses table isn't built yet, it just skips it)
		addrRows, err := app.DB.Query(ctx, "SELECT id, full_name, address_line1, city, postal_code, is_default_shipping FROM addresses WHERE user_id = $1", userID)
		if err == nil {
			defer addrRows.Close()
			for addrRows.Next() {
				var a Address
				if err := addrRows.Scan(&a.Id, &a.Name, &a.Street, &a.City, &a.Zip, &a.Default); err == nil {
					addresses = append(addresses, a)
				}
			}
		}
	}

	// Ensure nil slices are formatted as empty arrays for the HTML Template
	if activeItems == nil {
		activeItems = []CheckoutViewItem{}
	}
	if addresses == nil {
		addresses = []Address{}
	}

	pageData := struct {
		Items     []CheckoutViewItem
		Addresses []Address
	}{
		Items:     activeItems,
		Addresses: addresses,
	}

	app.renderPage(w, r, pageData, "templates/checkout.html")
}

// --- Profile Routes ---

func (app *App) Profile(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	cookie, err := r.Cookie("session_token")
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	email, err := app.GetUserFromSession(ctx, cookie.Value)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	var userID int64
	var name string
	err = app.DB.QueryRow(ctx, "SELECT id, name FROM users WHERE email = $1", email).Scan(&userID, &name)
	if err != nil {
		app.Logger.Error("Failed to fetch user profile data", "error", err)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	cookieHash := sha256.Sum256([]byte(cookie.Value))
	currentSessionHash := hex.EncodeToString(cookieHash[:])

	data := ProfileResponse{
		Name:           name,
		Email:          email,
		Sessions:       app.fetchProfileSessions(ctx, userID, currentSessionHash),
		Addresses:      app.fetchProfileAddresses(ctx, userID),
		PaymentMethods: []PaymentMethod{},
	}
	app.renderPage(w, r, data, "templates/layouts/profilenav.html", "templates/profile-index.html")
}

type OrderHistoryItem struct {
	ID     string
	Date   string
	Status string
	Total  string
}

type ProfileOrdersResponse struct {
	Orders []OrderHistoryItem
}

func (app *App) ProfileOrders(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	userID, err := app.validateUserSession(r, ctx)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	var orders []OrderHistoryItem
	rows, err := app.DB.Query(ctx, "SELECT id, created_at, status, total_amount FROM orders WHERE user_id = $1 ORDER BY created_at DESC", userID)

	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var o OrderHistoryItem
			var createdAt time.Time
			var total float64
			if err := rows.Scan(&o.ID, &createdAt, &o.Status, &total); err == nil {
				o.Date = createdAt.Format("Jan 02, 2006")
				o.Total = fmt.Sprintf("$%.2f", total)
				orders = append(orders, o)
			}
		}
	}

	if orders == nil {
		orders = []OrderHistoryItem{}
	}

	data := ProfileOrdersResponse{Orders: orders}
	app.renderPage(w, r, data, "templates/layouts/profilenav.html", "templates/profile-orders.html")
}

type OrderItem struct {
	ID       int64
	Name     string
	Variant  string
	Price    string
	Quantity int
	Image    string
}

type OrderCosts struct {
	Subtotal string
	Shipping string
	Tax      string
	Total    string
}

type OrderAddress struct {
	Name    string
	Street  string
	City    string
	State   string
	Zip     string
	Country string
}

type OrderPayment struct {
	Name   string
	Expiry string
}

type TimelineStep struct {
	Status string
	Date   string
	Active bool
}

type OrderDetailResponse struct {
	ID              string
	PlacedAt        string
	Status          string
	Items           []OrderItem
	Costs           OrderCosts
	ShippingAddress OrderAddress
	PaymentMethod   OrderPayment
	Timeline        []TimelineStep
}

func (app *App) ProfileOrderDetail(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// 1. Verify User Access
	userID, err := app.validateUserSession(r, ctx)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	orderID := r.PathValue("orderID")
	var data OrderDetailResponse
	var createdAt time.Time
	var totalAmount float64
	var customerName string

	// 2. Fetch Core Order Details
	err = app.DB.QueryRow(ctx, `
		SELECT id, created_at, status, total_amount, customer_name
		FROM orders 
		WHERE id = $1 AND user_id = $2
	`, orderID, userID).Scan(&data.ID, &createdAt, &data.Status, &totalAmount, &customerName)

	if err != nil {
		app.Logger.Warn("Order not found or unauthorized", "orderID", orderID, "userID", userID)
		http.Redirect(w, r, "/profile/orders", http.StatusSeeOther)
		return
	}

	data.PlacedAt = createdAt.Format("Jan 02, 2006 at 3:04 PM")

	// 3. Fetch Order Items & Product Details
	itemRows, err := app.DB.Query(ctx, `
		SELECT oi.id, p.name, oi.price, oi.quantity, p.gallery, p.variations
		FROM order_items oi
		JOIN products p ON oi.product_id = p.id
		WHERE oi.order_id = $1
	`, orderID)

	var subtotal float64
	if err == nil {
		defer itemRows.Close()
		for itemRows.Next() {
			var item OrderItem
			var rawPrice float64
			var galleryJSON, varsJSON []byte

			err := itemRows.Scan(&item.ID, &item.Name, &rawPrice, &item.Quantity, &galleryJSON, &varsJSON)
			if err == nil {
				item.Price = fmt.Sprintf("$%.2f", rawPrice)
				subtotal += rawPrice * float64(item.Quantity)

				// Extract Image
				var gallery []string
				_ = json.Unmarshal(galleryJSON, &gallery)
				if len(gallery) > 0 {
					item.Image = gallery[0]
				} else {
					item.Image = "https://placehold.co/100x100?text=No+Image"
				}

				// Extract Variations into a single string (e.g., "Color: Red, Size: L")
				var variations map[string]string
				_ = json.Unmarshal(varsJSON, &variations)
				var variantParts []string
				for k, v := range variations {
					variantParts = append(variantParts, fmt.Sprintf("%s: %s", k, v))
				}
				item.Variant = strings.Join(variantParts, ", ")

				data.Items = append(data.Items, item)
			}
		}
	}

	if data.Items == nil {
		data.Items = []OrderItem{}
	}

	// 4. Calculate Costs
	// We reverse-engineer the tax/shipping based on subtotal to match checkout logic
	tax := subtotal * 0.08
	shipping := totalAmount - subtotal - tax
	if shipping < 0 {
		shipping = 0
	}

	data.Costs = OrderCosts{
		Subtotal: fmt.Sprintf("$%.2f", subtotal),
		Shipping: fmt.Sprintf("$%.2f", shipping),
		Tax:      fmt.Sprintf("$%.2f", tax),
		Total:    fmt.Sprintf("$%.2f", totalAmount),
	}

	// 5. Fetch Payment Info (If exists)
	var paymentIntent string
	err = app.DB.QueryRow(ctx, "SELECT stripe_payment_intent_id FROM payments WHERE order_id = $1 LIMIT 1", orderID).Scan(&paymentIntent)
	if err == nil && paymentIntent != "" {
		data.PaymentMethod = OrderPayment{Name: "Paid via Stripe", Expiry: "Secure Token"}
	} else {
		data.PaymentMethod = OrderPayment{Name: "Unknown", Expiry: ""}
	}

	// 6. Build the Timeline Logic
	// In a real app, this would query a separate `order_events` table.
	// Here, we derive it dynamically based on the current Order Status.
	data.Timeline = []TimelineStep{
		{"Ordered", createdAt.Format("Jan 02"), true},
		{"Processing", "Pending", data.Status == "Processing" || data.Status == "Shipped" || data.Status == "Delivered"},
		{"Shipped", "Pending", data.Status == "Shipped" || data.Status == "Delivered"},
		{"Delivered", "Pending", data.Status == "Delivered"},
	}

	// 7. Mock Address (Until you hook up the addresses table to the checkout API)
	data.ShippingAddress = OrderAddress{
		Name:    customerName,
		Street:  "Shipping Address Provided",
		City:    "To Stripe",
		State:   "During Checkout",
		Zip:     "",
		Country: "",
	}

	app.renderPage(w, r, data, "templates/layouts/profilenav.html", "templates/profile-order-detail.html")
}

type ProfileSupportResponse struct {
	Name    string              `json:"name"`
	Email   string              `json:"email"`
	Tickets []UserSupportTicket `json:"tickets"`
}

type UserSupportTicket struct {
	ID          string `json:"id"`
	Subject     string `json:"subject"`
	Message     string `json:"message"`
	Status      string `json:"status"`
	LastUpdated string `json:"lastUpdated"`
}

// --- Support Handler ---
func (app *App) ProfileSupport(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	cookie, err := r.Cookie("session_token")
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	email, err := app.GetUserFromSession(ctx, cookie.Value)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	// Get the user's name for the ticket submission form
	var name string
	err = app.DB.QueryRow(ctx, "SELECT name FROM users WHERE email = $1", email).Scan(&name)
	if err != nil {
		name = "Customer"
	}

	// Fetch all tickets matching their email
	rows, err := app.DB.Query(ctx, `
		SELECT id, subject, message, status, created_at 
		FROM support_tickets 
		WHERE email = $1 
		ORDER BY created_at DESC
	`, email)

	var tickets []UserSupportTicket
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var t UserSupportTicket
			var rawID int
			var createdAt time.Time

			if err := rows.Scan(&rawID, &t.Subject, &t.Message, &t.Status, &createdAt); err == nil {
				t.ID = fmt.Sprintf("TKT-%04d", rawID)
				t.Status = strings.ToUpper(t.Status[:1]) + t.Status[1:] // Capitalize status
				t.LastUpdated = TimeAgo(createdAt)
				tickets = append(tickets, t)
			}
		}
	} else {
		app.Logger.Error("Failed to fetch user support tickets", "error", err)
	}

	if tickets == nil {
		tickets = []UserSupportTicket{}
	}

	data := ProfileSupportResponse{
		Name:    name,
		Email:   email,
		Tickets: tickets,
	}

	app.renderPage(w, r, data, "templates/layouts/profilenav.html", "templates/profile-support.html")
}

func TimeAgo(t time.Time) string {
	d := time.Since(t)

	if d.Seconds() < 60 {
		return "just now"
	}
	if d.Minutes() < 60 {
		minutes := int(d.Minutes())
		if minutes == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", minutes)
	}
	if d.Hours() < 24 {
		hours := int(d.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	}
	days := int(d.Hours() / 24)
	if days == 1 {
		return "1 day ago"
	}
	return fmt.Sprintf("%d days ago", days)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) &&
		(len(substr) == 0 || (func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				match := true
				for j := range len(substr) {
					if toLower(s[i+j]) != toLower(substr[j]) {
						match = false
						break
					}
				}
				if match {
					return true
				}
			}
			return false
		})())
}

func toLower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// ---------------------------------------------------------
// DATABASE & SESSION LOGIC
// ---------------------------------------------------------

func (app *App) GetUserFromSession(ctx context.Context, token string) (string, error) {
	hash := sha256.Sum256([]byte(token))
	hashedToken := hex.EncodeToString(hash[:])

	var email string
	err := app.DB.QueryRow(ctx, "SELECT u.email FROM sessions s JOIN users u ON s.user_id = u.id WHERE s.token = $1", hashedToken).Scan(&email)
	if err != nil {
		return "", fmt.Errorf("session token query: %w", err)
	}
	return email, nil
}

func (app *App) validateUserSession(r *http.Request, ctx context.Context) (int64, error) {
	cookie, err := r.Cookie("session_token")
	if err != nil {
		return 0, fmt.Errorf("session cookie missing: %w", err)
	}

	hash := sha256.Sum256([]byte(cookie.Value))
	hashedToken := hex.EncodeToString(hash[:])

	var userID int64
	var expires int64
	err = app.DB.QueryRow(ctx, "SELECT user_id, expires FROM sessions WHERE token = $1", hashedToken).Scan(&userID, &expires)
	if err != nil {
		app.Logger.Error("DB session lookup failed", "error", err, "token_hash", hashedToken)
		return 0, fmt.Errorf("querying session: %w", err)
	}
	if time.Now().Unix() > expires {
		_, _ = app.DB.Exec(ctx, "DELETE FROM sessions WHERE token = $1", hashedToken)
		return 0, fmt.Errorf("session check: %w", sql.ErrNoRows)
	}
	return userID, nil
}

func (app *App) renderPage(w http.ResponseWriter, r *http.Request, pageData any, pageTmpl ...string) {
	query := r.URL.Query().Get("q")
	var isEmployee bool
	var isLoggedIn bool

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	cookie, err := r.Cookie("session_token")
	if err == nil {
		if email, err := app.GetUserFromSession(ctx, cookie.Value); err == nil {
			isLoggedIn = true
			var empID int
			err := app.DB.QueryRow(ctx, `
				SELECT id 
				FROM employees 
				WHERE email = $1 AND status = 'active'
			`, email).Scan(&empID)

			if err == nil {
				isEmployee = true
			}
		}
	}

	data := struct {
		AppVersion string
		Query      string
		IsLoggedIn bool
		IsEmployee bool
		PageData   any
	}{
		AppVersion: app.AppVersion,
		Query:      query,
		IsLoggedIn: isLoggedIn,
		IsEmployee: isEmployee,
		PageData:   pageData,
	}

	cacheKey := strings.Join(pageTmpl, ",")

	app.cacheMutex.RLock()
	tmpl, exists := app.templateCache[cacheKey]
	app.cacheMutex.RUnlock()

	if !exists {
		app.cacheMutex.Lock()
		tmpl, exists = app.templateCache[cacheKey]
		if !exists {
			funcMap := template.FuncMap{
				"timeAgo": TimeAgo,
			}

			files := append([]string{"templates/layouts/shell.html"}, pageTmpl...)
			var err error
			tmpl, err = template.New("shell.html").Funcs(funcMap).ParseFS(app.Templates, files...)
			if err != nil {
				app.cacheMutex.Unlock()
				app.Logger.Error("template parsing error", "error", err, "templates", files)
				http.Error(w, "Error parsing templates", http.StatusInternalServerError)
				return
			}

			if app.templateCache == nil {
				app.templateCache = make(map[string]*template.Template)
			}
			app.templateCache[cacheKey] = tmpl
			app.Logger.Info("Template parsed and cached", "key", cacheKey)
		}
		app.cacheMutex.Unlock()
	}

	var buf bytes.Buffer
	err = tmpl.ExecuteTemplate(&buf, "shell.html", data)
	if err != nil {
		app.Logger.Error("template execution error", "error", err, "templates", pageTmpl)
		http.Error(w, "Error executing template", http.StatusInternalServerError)
		return
	}

	hash := sha256.Sum256(buf.Bytes())
	etag := fmt.Sprintf(`W/"%x"`, hash)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "max-age=0, private, must-revalidate")
	w.Header().Set("Vary", "Cookie")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	_, err = buf.WriteTo(w)
	if err != nil {
		app.Logger.Error("response write error", "error", err)
		http.Error(w, "Error executing template", http.StatusInternalServerError)
		return
	}
}

func (app *App) fetchProfileAddresses(ctx context.Context, userID int64) []SavedAddress {
	var addresses []SavedAddress
	addressRows, err := app.DB.Query(ctx, "SELECT id, full_name, address_line1, city, postal_code, is_default_shipping FROM addresses WHERE user_id = $1", userID)
	if err != nil {
		app.Logger.Error("failed to query addresses", "error", err)
		return addresses
	}
	defer addressRows.Close()

	for addressRows.Next() {
		var addr SavedAddress
		var fullName, street, city, zip sql.NullString
		var isDefault bool

		err := addressRows.Scan(&addr.ID, &fullName, &street, &city, &zip, &isDefault)
		if err != nil {
			app.Logger.Error("failed to scan address", "error", err)
			continue
		}

		addr.Label = fullName.String
		if addr.Label == "" {
			addr.Label = "Saved Address"
		}
		addr.Street = street.String
		addr.City = city.String
		addr.Zip = zip.String
		addr.IsDefault = isDefault
		addresses = append(addresses, addr)
	}

	if err := addressRows.Err(); err != nil {
		app.Logger.Error("address rows error", "error", err)
	}

	return addresses
}

func (app *App) fetchProfileSessions(ctx context.Context, userID int64, currentSessionHash string) []Session {
	var sessions []Session
	sessionRows, err := app.DB.Query(ctx, "SELECT token, os, browser, location, created_at FROM sessions WHERE user_id = $1 ORDER BY created_at DESC", userID)
	if err != nil {
		app.Logger.Error("failed to query sessions", "error", err)
		return sessions
	}
	defer sessionRows.Close()

	for sessionRows.Next() {
		var token, osName, browser, loc string
		var createdAt time.Time

		if err := sessionRows.Scan(&token, &osName, &browser, &loc, &createdAt); err != nil {
			app.Logger.Error("failed to scan session", "error", err)
			continue
		}
		sessions = append(sessions, Session{
			Token:     token,
			OS:        osName,
			Browser:   browser,
			Location:  loc,
			LoginDate: createdAt.Format("Jan 02, 2006 at 3:04 PM"),
			IsCurrent: token == currentSessionHash,
		})
	}

	if err := sessionRows.Err(); err != nil {
		app.Logger.Error("session rows error", "error", err)
	}

	return sessions
}

type Category struct {
	ID   int
	Name string
	Slug string
	Icon string
}
type Products struct {
	ID            int
	Name          string
	Price         int
	DiscountPrice *int
}
type CheckoutProduct struct {
	ProductId       int
	Name            string
	Price           float64
	Image           string
	ShippingOptions []ShippingOption
}
type ShippingOption struct {
	Id    string
	Name  string
	Price float64
	Desc  string
}
type Address struct {
	Id      int
	Name    string
	Street  string
	City    string
	Zip     string
	Default bool
}
type SearchProduct struct {
	ID                int
	Name              string
	Category          string
	Price             int
	DiscountPrice     *int
	Rating            float64
	Reviews           int
	NonMemberDelivery string
}
type StoreDetail struct {
	Name       string
	Logo       string
	Rating     float64
	Reviews    int
	JoinedDate string
}

type Variation struct {
	Name    string
	Options []string
}
type Addon struct {
	ID          string
	Name        string
	Description string
	Price       float64
}

type VariationGroup struct {
	Name    string
	Options []string
}

type ReviewStat struct {
	Stars      int
	Percentage int
	Count      int
}

type DetailReview struct {
	UserName   string
	Initials   string
	Date       string
	IsVerified bool
	Rating     int
	Title      string
	Comment    string
}

type ProductDetailResponse struct {
	ID              int64
	Name            string
	Price           float64
	DiscountPrice   *float64
	Description     string
	Gallery         []string
	Rating          float64
	ReviewsCount    int
	Variations      []VariationGroup
	CurrentVarsJSON string // Passed to frontend JS
	SiblingsJSON    string // Passed to frontend JS
	ReviewStats     []ReviewStat
	Reviews         []DetailReview
}
type ProfileResponse struct {
	Name           string
	Business       int
	Email          string
	Credentials    []Credential
	Sessions       []Session
	Addresses      []SavedAddress
	PaymentMethods []PaymentMethod
}
type Credential struct {
	ID   string
	Name string
}
type Session struct {
	Token     string
	OS        string
	Browser   string
	Location  string
	LoginDate string
	IsCurrent bool
}
type SavedAddress struct {
	ID        int
	Label     string
	Street    string
	City      string
	Zip       string
	IsDefault bool
}
type PaymentMethod struct {
	ID        int
	Type      string
	Name      string
	Details   string
	IsDefault bool
}
type ProfileSettingsResponse struct {
	Profile SettingsUser
	Plans   []Subscription
}
type SettingsUser struct {
	Business     int
	Subscription string
}
type Subscription struct {
	ID       string
	Name     string
	Price    int
	PerUser  bool
	Features []string
}
