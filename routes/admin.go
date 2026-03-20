package routes

import (
	"context"
	"database/sql"
	"ecommerce/api"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type DashboardStats struct {
	Revenue       string
	RevenueGrowth string
	Sales         string
	SalesGrowth   string
	Products      string
}

type DashboardProduct struct {
	ID           int64
	Name         string
	BusinessName string
	Price        float64
	Stock        int
	Rating       float64
	Updated      string
	Image        string
}

type DashboardDataResponse struct {
	Stats    DashboardStats
	Products []DashboardProduct
}

func (app *App) AdminDashboard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel, err := app.authorizeAdmin(w, r)
	if err != nil {
		cancel()
		return
	}
	defer cancel()

	var stats DashboardStats

	// 1. Get Live Active Products Count
	var productCount int
	_ = app.DB.QueryRow(ctx, "SELECT COUNT(*) FROM products WHERE is_active = true").Scan(&productCount)
	stats.Products = strconv.Itoa(productCount)

	// 2. Get Revenue & Sales (Safe query in case orders table isn't built yet)
	var totalSales int
	var totalRevenue float64
	err = app.DB.QueryRow(ctx, "SELECT COUNT(*), COALESCE(SUM(total_amount), 0) FROM orders WHERE status != 'cancelled'").Scan(&totalSales, &totalRevenue)
	if err != nil {
		// Fallback if orders table doesn't exist yet
		stats.Sales = "0"
		stats.Revenue = "$0.00"
		stats.SalesGrowth = "0%"
		stats.RevenueGrowth = "0%"
	} else {
		stats.Sales = strconv.Itoa(totalSales)
		stats.Revenue = fmt.Sprintf("$%.2f", totalRevenue)
		stats.SalesGrowth = "+0.0%" // Placeholder until historical tracking logic is added
		stats.RevenueGrowth = "+0.0%"
	}

	// 3. Fetch Recent Products & Calculate Average Ratings
	query := `
		SELECT p.id, p.name, p.price, p.stock, p.gallery, p.updated_at, 
		       COALESCE(AVG(r.rating), 0.0) as rating
		FROM products p
		LEFT JOIN reviews r ON p.id = r.product_id
		GROUP BY p.id
		ORDER BY p.updated_at DESC
		LIMIT 50
	`

	rows, err := app.DB.Query(ctx, query)
	var products []DashboardProduct

	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var p DashboardProduct
			var galleryJSON []byte
			var updatedAt time.Time

			if err := rows.Scan(&p.ID, &p.Name, &p.Price, &p.Stock, &galleryJSON, &updatedAt, &p.Rating); err == nil {
				// Parse the first image from the JSONB gallery array
				var gallery []string
				_ = json.Unmarshal(galleryJSON, &gallery)
				if len(gallery) > 0 && gallery[0] != "" {
					p.Image = gallery[0]
				} else {
					p.Image = "https://placehold.co/100x100?text=No+Image"
				}

				p.Updated = updatedAt.Format("Jan 02, 2006")
				p.BusinessName = "My Store" // Hardcoded since we moved to a single-store schema

				products = append(products, p)
			}
		}
	} else {
		app.Logger.Error("Failed to fetch dashboard products", "error", err)
	}

	if products == nil {
		products = []DashboardProduct{}
	}

	data := DashboardDataResponse{
		Stats:    stats,
		Products: products,
	}

	app.renderPage(w, r, data, "templates/layouts/sellernav.html", "templates/dashboard.html")
}

type InventoryShipping struct {
	IsFree bool `json:"isFree"`
}

type InventoryProduct struct {
	ID            int64             `json:"id"`
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	Price         float64           `json:"price"`
	DiscountPrice *float64          `json:"discountPrice"`
	Stock         int               `json:"stock"`
	Gallery       []string          `json:"gallery"`
	Variations    map[string]string `json:"variations"`
	Shipping      InventoryShipping `json:"shipping"`
}

type InventoryGroup struct {
	ID             int64              `json:"id"`
	Name           string             `json:"name"`
	VariationTypes []string           `json:"variationTypes"`
	Expanded       bool               `json:"expanded"`
	Products       []InventoryProduct `json:"products"`
}

func (app *App) AdminInventory(w http.ResponseWriter, r *http.Request) {
	ctx, cancel, err := app.authorizeAdmin(w, r)
	if err != nil {
		cancel()
		return
	}
	defer cancel()

	// 1. Fetch all product groups
	groupRows, err := app.DB.Query(ctx, "SELECT id, name, variation_types FROM product_groups ORDER BY created_at DESC")
	if err != nil {
		app.Logger.Error("Failed to fetch product groups", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer groupRows.Close()

	groupsMap := make(map[int64]*InventoryGroup)
	var groups []InventoryGroup

	for groupRows.Next() {
		var g InventoryGroup
		var vTypesJSON []byte
		if err := groupRows.Scan(&g.ID, &g.Name, &vTypesJSON); err == nil {
			_ = json.Unmarshal(vTypesJSON, &g.VariationTypes)
			g.Products = []InventoryProduct{} // Initialize empty slice
			g.Expanded = false
			groupsMap[g.ID] = &g
			groups = append(groups, g) // Keep ordered slice
		}
	}

	// 2. Fetch all products
	prodRows, err := app.DB.Query(ctx, `
		SELECT id, group_id, name, description, price, discount_price, stock, gallery, variations, is_free_shipping 
		FROM products 
		ORDER BY created_at DESC
	`)

	if err == nil {
		defer prodRows.Close()
		for prodRows.Next() {
			var p InventoryProduct
			var groupID int64
			var galleryJSON, variationsJSON []byte
			var isFreeShipping bool

			err := prodRows.Scan(&p.ID, &groupID, &p.Name, &p.Description, &p.Price, &p.DiscountPrice, &p.Stock, &galleryJSON, &variationsJSON, &isFreeShipping)
			if err == nil {
				_ = json.Unmarshal(galleryJSON, &p.Gallery)
				_ = json.Unmarshal(variationsJSON, &p.Variations)
				p.Shipping = InventoryShipping{IsFree: isFreeShipping}

				// Attach to correct group
				if groupPtr, exists := groupsMap[groupID]; exists {
					groupPtr.Products = append(groupPtr.Products, p)
				}
			}
		}
	}

	// Re-map the pointers back to the final slice
	var finalGroups []InventoryGroup
	for _, g := range groups {
		finalGroups = append(finalGroups, *groupsMap[g.ID])
	}

	if finalGroups == nil {
		finalGroups = []InventoryGroup{}
	}

	app.renderPage(w, r, finalGroups, "templates/layouts/sellernav.html", "templates/inventory.html")
}

type EditProductViewData struct {
	ID             int64
	GroupID        int64
	GroupName      string
	VariationTypes []string
	Name           string
	Description    string
	Price          float64
	DiscountPrice  *float64
	Stock          int
	Gallery        []string
	Variations     map[string]string
	IsFreeShipping bool
}

func (app *App) AdminProductEdit(w http.ResponseWriter, r *http.Request) {
	ctx, cancel, err := app.authorizeAdmin(w, r)
	if err != nil {
		cancel()
		return
	}
	defer cancel()

	productID := r.PathValue("productID")
	var data EditProductViewData
	var galleryJSON, varsJSON, typesJSON []byte

	query := `
		SELECT p.id, p.group_id, g.name, g.variation_types, p.name, p.description, 
		       p.price, p.discount_price, p.stock, p.gallery, p.variations, p.is_free_shipping
		FROM products p
		JOIN product_groups g ON p.group_id = g.id
		WHERE p.id = $1
	`
	err = app.DB.QueryRow(ctx, query, productID).Scan(
		&data.ID, &data.GroupID, &data.GroupName, &typesJSON, &data.Name, &data.Description,
		&data.Price, &data.DiscountPrice, &data.Stock, &galleryJSON, &varsJSON, &data.IsFreeShipping,
	)

	if err != nil {
		app.Logger.Error("Product not found", "error", err)
		http.Redirect(w, r, "/admin/products", http.StatusSeeOther)
		return
	}

	json.Unmarshal(typesJSON, &data.VariationTypes)
	json.Unmarshal(galleryJSON, &data.Gallery)
	json.Unmarshal(varsJSON, &data.Variations)

	if data.Gallery == nil {
		data.Gallery = []string{""}
	}
	if data.VariationTypes == nil {
		data.VariationTypes = []string{}
	}
	if data.Variations == nil {
		data.Variations = map[string]string{}
	}

	app.renderPage(w, r, data, "templates/layouts/sellernav.html", "templates/product-edit.html")
}

type AdminOrderView struct {
	ID        string
	Customer  string
	Email     string
	Date      string
	Status    string
	Total     string
	ItemCount int
}

func (app *App) AdminOrders(w http.ResponseWriter, r *http.Request) {
	ctx, cancel, err := app.authorizeAdmin(w, r)
	if err != nil {
		cancel()
		return
	}
	defer cancel()

	query := `
		SELECT o.id, o.customer_name, o.customer_email, o.created_at, o.status, o.total_amount, 
		       COALESCE(SUM(oi.quantity), 0) as item_count
		FROM orders o
		LEFT JOIN order_items oi ON o.id = oi.order_id
		GROUP BY o.id
		ORDER BY o.created_at DESC
	`

	rows, err := app.DB.Query(ctx, query)
	var orders []AdminOrderView

	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var o AdminOrderView
			var createdAt time.Time
			var total float64
			if err := rows.Scan(&o.ID, &o.Customer, &o.Email, &createdAt, &o.Status, &total, &o.ItemCount); err == nil {
				o.Date = createdAt.Format("Jan 02, 2006")
				o.Total = fmt.Sprintf("$%.2f", total)
				orders = append(orders, o)
			}
		}
	}

	if orders == nil {
		orders = []AdminOrderView{}
	}

	app.renderPage(w, r, orders, "templates/layouts/sellernav.html", "templates/orders.html")
}

type SellerOrderItem struct {
	ID       int64
	Name     string
	SKU      string
	Price    float64
	Quantity int
	Image    string
}

type SellerOrderEvent struct {
	Title       string
	Description string
	Time        string
	Active      bool
}

type SellerOrderCustomer struct {
	Name               string
	Email              string
	Phone              string
	PreviousOrderCount int
}

type SellerOrderPricing struct {
	Subtotal float64
	Shipping float64
	Tax      float64
	Total    float64
}

type SellerOrderPayment struct {
	Method string
	Last4  string
}

type SellerOrderDetailResponse struct {
	ID              string
	Status          string
	Date            string
	Time            string
	Items           []SellerOrderItem
	Timeline        []SellerOrderEvent
	Customer        SellerOrderCustomer
	ShippingAddress string
	BillingAddress  string
	Pricing         SellerOrderPricing
	Payment         SellerOrderPayment
}

func (app *App) AdminOrderDetail(w http.ResponseWriter, r *http.Request) {
	ctx, cancel, err := app.authorizeAdmin(w, r)
	if err != nil {
		cancel()
		return
	}
	defer cancel()

	orderID := r.PathValue("orderID")
	var data SellerOrderDetailResponse
	var createdAt time.Time
	var userID *int64

	// 1. Fetch Core Order Details
	err = app.DB.QueryRow(ctx, `
		SELECT id, created_at, status, total_amount, customer_name, customer_email, user_id
		FROM orders 
		WHERE id = $1
	`, orderID).Scan(&data.ID, &createdAt, &data.Status, &data.Pricing.Total, &data.Customer.Name, &data.Customer.Email, &userID)

	if err != nil {
		app.Logger.Warn("Order not found in admin panel", "orderID", orderID)
		http.Redirect(w, r, "/admin/orders", http.StatusSeeOther)
		return
	}

	data.Date = createdAt.Format("Jan 02, 2006")
	data.Time = createdAt.Format("3:04 PM")
	data.Customer.Phone = "N/A" // Placeholder until phone numbers are added to checkout

	// 2. Determine Previous Orders
	// We use the email to count so it tracks Guest checkouts too!
	app.DB.QueryRow(ctx, "SELECT COUNT(*) FROM orders WHERE customer_email = $1", data.Customer.Email).Scan(&data.Customer.PreviousOrderCount)
	if data.Customer.PreviousOrderCount > 0 {
		data.Customer.PreviousOrderCount-- // Don't count the current order being viewed
	}

	// 3. Fetch Order Items
	itemRows, err := app.DB.Query(ctx, `
		SELECT oi.id, p.name, oi.price, oi.quantity, p.gallery, p.variations
		FROM order_items oi
		JOIN products p ON oi.product_id = p.id
		WHERE oi.order_id = $1
	`, orderID)

	if err == nil {
		defer itemRows.Close()
		for itemRows.Next() {
			var item SellerOrderItem
			var galleryJSON, varsJSON []byte

			err := itemRows.Scan(&item.ID, &item.Name, &item.Price, &item.Quantity, &galleryJSON, &varsJSON)
			if err == nil {
				data.Pricing.Subtotal += item.Price * float64(item.Quantity)

				// Extract Image
				var gallery []string
				_ = json.Unmarshal(galleryJSON, &gallery)
				if len(gallery) > 0 && gallery[0] != "" {
					item.Image = gallery[0]
				} else {
					item.Image = "https://placehold.co/100x100?text=No+Image"
				}

				// Build SKU dynamically from variations
				var variations map[string]string
				_ = json.Unmarshal(varsJSON, &variations)
				var variantParts []string
				for _, v := range variations {
					variantParts = append(variantParts, strings.ToUpper(v[:1]))
				}
				item.SKU = fmt.Sprintf("PRD-%d-%s", item.ID, strings.Join(variantParts, ""))
				if item.SKU == fmt.Sprintf("PRD-%d-", item.ID) {
					item.SKU = fmt.Sprintf("PRD-%d-STD", item.ID)
				}

				data.Items = append(data.Items, item)
			}
		}
	}

	if data.Items == nil {
		data.Items = []SellerOrderItem{}
	}

	// 4. Calculate Pricing Breakdown
	data.Pricing.Tax = data.Pricing.Subtotal * 0.08
	data.Pricing.Shipping = data.Pricing.Total - data.Pricing.Subtotal - data.Pricing.Tax
	if data.Pricing.Shipping < 0 {
		data.Pricing.Shipping = 0
	}

	// 5. Fetch Payment Info
	var paymentIntent string
	err = app.DB.QueryRow(ctx, "SELECT stripe_payment_intent_id FROM payments WHERE order_id = $1 LIMIT 1", orderID).Scan(&paymentIntent)
	if err == nil && paymentIntent != "" {
		data.Payment.Method = "Stripe"
		data.Payment.Last4 = "****" // In a full app, you can query Stripe's API using the intent ID to get the real last 4 digits
	} else {
		data.Payment.Method = "Unknown"
		data.Payment.Last4 = "----"
	}

	// 6. Build Timeline
	data.Timeline = []SellerOrderEvent{
		{Title: "Order Placed", Description: "Customer successfully placed the order.", Time: data.Date + " " + data.Time, Active: true},
		{Title: "Processing", Description: "Payment confirmed. Preparing items.", Time: "Pending", Active: data.Status == "Processing" || data.Status == "Shipped" || data.Status == "Delivered"},
		{Title: "Shipped", Description: "Handed over to carrier.", Time: "Pending", Active: data.Status == "Shipped" || data.Status == "Delivered"},
		{Title: "Delivered", Description: "Package arrived at destination.", Time: "Pending", Active: data.Status == "Delivered"},
	}

	// 7. Mock Addresses
	// (Until the addresses table is fully linked to the order creation API)
	data.ShippingAddress = "Securely held by Stripe Checkout"
	data.BillingAddress = "Securely held by Stripe Checkout"

	app.renderPage(w, r, data, "templates/layouts/sellernav.html", "templates/order-detail.html")
}

func (app *App) AdminEmployees(w http.ResponseWriter, r *http.Request) {
	ctx, cancel, err := app.authorizeAdmin(w, r)
	if err != nil {
		cancel()
		return
	}
	defer cancel()

	rows, err := app.DB.Query(ctx, `
		SELECT e.id, COALESCE(u.name, 'Staff Member'), e.email, e.roles, e.status, e.joined_at 
		FROM employees e 
		LEFT JOIN users u ON e.user_id = u.id 
		ORDER BY e.created_at DESC`)

	var team []SellerTeamMember
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var tm SellerTeamMember
			var joinedAt sql.NullTime
			var rolesJSON []byte

			if err := rows.Scan(&tm.ID, &tm.Name, &tm.Email, &rolesJSON, &tm.Status, &joinedAt); err == nil {
				_ = json.Unmarshal(rolesJSON, &tm.Roles)
				if joinedAt.Valid {
					tm.LastActive = joinedAt.Time.Format("Jan 02, 2006")
				} else {
					tm.LastActive = "Pending"
				}
				team = append(team, tm)
			}
		}
	} else {
		app.Logger.Error("employee rows iteration error", "error", err)
	}

	data := struct {
		Roles     []string
		Employees []SellerTeamMember
	}{
		Roles:     api.AvailableRoles,
		Employees: team,
	}

	app.renderPage(w, r, data, "templates/layouts/sellernav.html", "templates/employees.html")
}

func (app *App) AdminEmployeeDetail(w http.ResponseWriter, r *http.Request) {
	ctx, cancel, err := app.authorizeAdmin(w, r)
	if err != nil {
		cancel()
		return
	}
	defer cancel()

	empIDStr := r.PathValue("employeeID")
	empID, err := strconv.ParseInt(empIDStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid employee ID", http.StatusBadRequest)
		return
	}

	var emp SellerEmployeeResponse
	var rolesJSON []byte
	var joinedAt sql.NullTime

	err = app.DB.QueryRow(ctx, `
		SELECT e.id, COALESCE(u.name, 'Staff Member'), u.email, e.roles, e.status, e.joined_at
		FROM employees e
		JOIN users u ON e.user_id = u.id
		WHERE e.id = $1
	`, empID).Scan(&emp.ID, &emp.Name, &emp.Email, &rolesJSON, &emp.Status, &joinedAt)

	if err != nil {
		http.Error(w, "Employee not found", http.StatusNotFound)
		return
	}

	_ = json.Unmarshal(rolesJSON, &emp.Roles)

	if joinedAt.Valid {
		emp.JoinedDate = joinedAt.Time.Format("Jan 02, 2006")
	} else {
		emp.JoinedDate = "Pending"
	}
	emp.LastActive = "Just now"
	emp.Activity = []EmployeeActivity{}

	data := struct {
		Employee SellerEmployeeResponse
		Roles    []string
	}{
		Employee: emp,
		Roles:    api.AvailableRoles,
	}

	app.renderPage(w, r, data, "templates/layouts/sellernav.html", "templates/employee-detail.html")
}

func (app *App) AdminSupport(w http.ResponseWriter, r *http.Request) {
	ctx, cancel, err := app.authorizeAdmin(w, r)
	if err != nil {
		cancel()
		return
	}
	defer cancel()

	// 1. Fetch live statistics
	var stats SupportStats
	err = app.DB.QueryRow(ctx, `
		SELECT 
			COUNT(*) FILTER (WHERE status = 'open'),
			COUNT(*) FILTER (WHERE status = 'pending'),
			COUNT(*) FILTER (WHERE status = 'resolved' AND DATE(created_at) = CURRENT_DATE)
		FROM support_tickets
	`).Scan(&stats.OpenTickets, &stats.PendingTickets, &stats.ResolvedToday)

	if err != nil {
		app.Logger.Error("Failed to fetch ticket stats", "error", err)
	}

	// 2. Fetch the 50 most recent tickets
	rows, err := app.DB.Query(ctx, `
		SELECT id, name, email, subject, message, status, priority, created_at 
		FROM support_tickets 
		ORDER BY created_at DESC 
		LIMIT 50
	`)

	var tickets []SupportTicket
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var t SupportTicket
			var createdAt time.Time
			var rawID int

			if err := rows.Scan(&rawID, &t.Customer, &t.Email, &t.Subject, &t.Message, &t.Status, &t.Priority, &createdAt); err == nil {
				// Format the ID nicely (e.g., TKT-0012) and use your TimeAgo helper
				t.ID = fmt.Sprintf("TKT-%04d", rawID)
				t.LastUpdated = TimeAgo(createdAt)

				// Capitalize the first letter of Status and Priority for the UI
				t.Status = strings.ToUpper(t.Status[:1]) + t.Status[1:]
				t.Priority = strings.ToUpper(t.Priority[:1]) + t.Priority[1:]

				tickets = append(tickets, t)
			}
		}
	} else {
		app.Logger.Error("Failed to fetch support tickets", "error", err)
	}

	if tickets == nil {
		tickets = []SupportTicket{}
	}

	data := SupportDataResponse{
		Stats:   stats,
		Tickets: tickets,
	}
	app.renderPage(w, r, data, "templates/layouts/sellernav.html", "templates/support.html")
}

func (app *App) authorizeAdmin(w http.ResponseWriter, r *http.Request) (context.Context, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)

	userID, err := app.validateUserSession(r, ctx)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return ctx, cancel, err
	}

	var isEmployee bool
	err = app.DB.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM employees WHERE user_id = $1 AND status = 'active')", userID).Scan(&isEmployee)
	if err != nil || !isEmployee {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return ctx, cancel, errors.New("unauthorized: not an active employee")
	}

	return ctx, cancel, nil
}

type TopProduct struct {
	Name  string
	Sales int
}
type CategorySplit struct {
	Name  string
	Value int
}

type InventoryAddon struct {
	Name  string
	Price float64
}

type SellerOrderListItem struct {
	ID        string
	Customer  string
	Email     string
	Date      string
	Status    string
	Total     string
	ItemCount int
}

type SellerTeamMember struct {
	ID          int64
	Name        string
	Email       string
	Roles       []string
	Status      string
	LastActive  string
	Permissions []string
}

type SellerEmployeeResponse struct {
	ID          int64
	Name        string
	Email       string
	Roles       []string
	Status      string
	JoinedDate  string
	LastActive  string
	Permissions []string
	Activity    []EmployeeActivity
}
type EmployeeActivity struct {
	ID     int64
	Action string
	Target string
	Date   string
}
type SellerStoreSettingsResponse struct {
	StoreName    string
	LogoPreview  string
	CoverPreview string
}
type SupportTicket struct {
	ID          string
	Customer    string
	Email       string
	OrderID     string
	Subject     string
	Message     string
	Status      string
	Priority    string
	LastUpdated string
}
type SupportStats struct {
	OpenTickets    int
	PendingTickets int
	ResolvedToday  int
}
type SupportDataResponse struct {
	Stats   SupportStats
	Tickets []SupportTicket
}
