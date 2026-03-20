package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
)

const (
	QueryTimeoutShort  = 3 * time.Second
	QueryTimeoutMedium = 5 * time.Second
	QueryTimeoutLong   = 10 * time.Second
)

// ==========================================
// ADMIN INVENTORY CREATION APIs
// ==========================================

type CreateGroupReq struct {
	Name           string   `json:"name" validate:"required"`
	VariationTypes []string `json:"variationTypes"`
}

func (app *App) CreateProductGroup(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), QueryTimeoutShort)
	defer cancel()

	// Handled by middleware: RequirePermission("products") ensures user is authorized

	var req CreateGroupReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		http.Error(w, "Group name required", http.StatusBadRequest)
		return
	}

	vTypesJSON, _ := json.Marshal(req.VariationTypes)

	var newID int64
	err := app.DB.QueryRow(ctx, `
		INSERT INTO product_groups (name, variation_types) 
		VALUES ($1, $2) RETURNING id
	`, req.Name, vTypesJSON).Scan(&newID)

	if err != nil {
		app.Logger.Error("Failed to create product group", "error", err)
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	app.WriteJSON(w, r, http.StatusOK, map[string]int64{"id": newID})
}

type CreateProductReq struct {
	GroupID       int64             `json:"groupId"`
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	Price         float64           `json:"price"`
	DiscountPrice *float64          `json:"discountPrice"`
	Stock         int               `json:"stock"`
	Gallery       []string          `json:"gallery"`
	Variations    map[string]string `json:"variations"`
	Shipping      struct {
		IsFree bool `json:"isFree"`
	} `json:"shipping"`
}

func (app *App) CreateProduct(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), QueryTimeoutShort)
	defer cancel()

	var req CreateProductReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	galleryJSON, _ := json.Marshal(req.Gallery)
	variationsJSON, _ := json.Marshal(req.Variations)

	var newID int64
	err := app.DB.QueryRow(ctx, `
		INSERT INTO products (group_id, name, description, price, discount_price, stock, gallery, variations, is_free_shipping) 
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id
	`, req.GroupID, req.Name, req.Description, req.Price, req.DiscountPrice, req.Stock, galleryJSON, variationsJSON, req.Shipping.IsFree).Scan(&newID)

	if err != nil {
		app.Logger.Error("Failed to create nested product", "error", err)
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	app.WriteJSON(w, r, http.StatusOK, map[string]int64{"id": newID})
}

// ==========================================
// PUBLIC STOREFRONT APIs
// ==========================================

type Product struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	Price         float64    `json:"price"`
	DiscountPrice *float64   `json:"discountPrice"`
	Stock         int        `json:"stock,omitempty"`
	Gallery       []string   `json:"gallery,omitempty"`
	UpdatedAt     *time.Time `json:"updatedAt,omitempty"`
}

func (app *App) ProductListAPI(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), QueryTimeoutShort)
	defer cancel()

	rows, err := app.DB.Query(ctx, `
		SELECT id, name, price, discount_price, gallery
		FROM products
		WHERE is_active = true
		ORDER BY created_at DESC
		LIMIT 36
	`)
	if err != nil {
		app.Logger.Error("Database query error in ProductListAPI", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var products []Product
	for rows.Next() {
		var p Product
		var galleryJSON []byte
		err := rows.Scan(&p.ID, &p.Name, &p.Price, &p.DiscountPrice, &galleryJSON)
		if err == nil {
			_ = json.Unmarshal(galleryJSON, &p.Gallery)
			products = append(products, p)
		}
	}

	if products == nil {
		products = []Product{}
	}
	app.WriteJSON(w, r, http.StatusOK, products)
}

func (app *App) StoreProductsAPI(w http.ResponseWriter, r *http.Request) {
	app.ProductListAPI(w, r)
}

func (app *App) SearchProductAPI(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	ctx, cancel := context.WithTimeout(r.Context(), QueryTimeoutShort)
	defer cancel()

	if query == "" {
		app.WriteJSON(w, r, http.StatusOK, []Product{})
		return
	}

	searchTerm := "%" + query + "%"
	rows, err := app.DB.Query(ctx, `
		SELECT id, name, price, discount_price, gallery
		FROM products
		WHERE is_active = true AND LOWER(name) LIKE LOWER($1)
		LIMIT 36`, searchTerm)

	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var products []Product
	for rows.Next() {
		var p Product
		var galleryJSON []byte
		if err := rows.Scan(&p.ID, &p.Name, &p.Price, &p.DiscountPrice, &galleryJSON); err == nil {
			_ = json.Unmarshal(galleryJSON, &p.Gallery)
			products = append(products, p)
		}
	}

	if products == nil {
		products = []Product{}
	}
	app.WriteJSON(w, r, http.StatusOK, products)
}

// ==========================================
// CART & CHECKOUT APIs
// ==========================================

type CartItem struct {
	ProductID string `json:"productId" validate:"required"`
	Quantity  int    `json:"quantity" validate:"required,min=1"`
}

type CartProduct struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	Price         float64  `json:"price"`
	DiscountPrice *float64 `json:"discountPrice"`
	Quantity      int      `json:"quantity"`
	TotalPrice    float64  `json:"totalPrice"`
	Image         string   `json:"image"`
}

func (app *App) SendCartData(w http.ResponseWriter, r *http.Request) {
	var cartItems []CartItem
	if err := json.NewDecoder(r.Body).Decode(&cartItems); err != nil || len(cartItems) == 0 {
		app.WriteJSON(w, r, http.StatusOK, []CartProduct{})
		return
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

	ctx, cancel := context.WithTimeout(r.Context(), QueryTimeoutShort)
	defer cancel()

	rows, err := app.DB.Query(ctx, "SELECT id, name, price, discount_price, gallery FROM products WHERE id = ANY($1)", ids)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var response []CartProduct
	for rows.Next() {
		var p Product
		var galleryJSON []byte
		if err := rows.Scan(&p.ID, &p.Name, &p.Price, &p.DiscountPrice, &galleryJSON); err == nil {
			_ = json.Unmarshal(galleryJSON, &p.Gallery)

			qty := quantityMap[p.ID]
			activePrice := p.Price
			if p.DiscountPrice != nil {
				activePrice = *p.DiscountPrice
			}

			img := ""
			if len(p.Gallery) > 0 {
				img = p.Gallery[0]
			}

			response = append(response, CartProduct{
				ID:            p.ID,
				Name:          p.Name,
				Price:         p.Price,
				DiscountPrice: p.DiscountPrice,
				Quantity:      qty,
				TotalPrice:    activePrice * float64(qty),
				Image:         img,
			})
		}
	}

	if response == nil {
		response = []CartProduct{}
	}
	app.WriteJSON(w, r, http.StatusOK, response)
}

func (app *App) SendCheckoutData(w http.ResponseWriter, r *http.Request) {
	app.SendCartData(w, r) // Logic is identical for processing totals
}

// ==========================================
// REVIEWS
// ==========================================
type ReviewForm struct {
	Review string `validate:"required,min=1"`
	Rating int    `validate:"required,min=1,max=5"`
}

func (app *App) Review(w http.ResponseWriter, r *http.Request) {
	// Keep existing review logic...
	ctx, cancel := context.WithTimeout(r.Context(), QueryTimeoutShort)
	defer cancel()

	productID, _ := strconv.Atoi(r.PathValue("productID"))
	userID, err := app.validateUserSession(r, ctx)
	if err != nil {
		http.Error(w, "Invalid session", http.StatusUnauthorized)
		return
	}

	rating, _ := strconv.Atoi(r.PostFormValue("rating"))
	form := ReviewForm{Review: strings.TrimSpace(r.PostFormValue("review")), Rating: rating}

	var validate = validator.New()
	if err = validate.Struct(form); err != nil {
		http.Error(w, "Validation Error: Invalid input", http.StatusBadRequest)
		return
	}

	_, err = app.DB.Exec(ctx, `INSERT INTO reviews (user_id, product_id, comment, rating) VALUES ($1, $2, $3, $4)`, userID, productID, form.Review, form.Rating)
	if err != nil {
		http.Error(w, "Failed to create review", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (app *App) UpdateProduct(w http.ResponseWriter, r *http.Request) {
	ctx, cancel, err := app.authorizeAdmin(w, r)
	if err != nil {
		cancel()
		return
	}
	defer cancel()

	productID := r.PathValue("productID")

	var req struct {
		Name          string            `json:"name"`
		Description   string            `json:"description"`
		Price         float64           `json:"price"`
		DiscountPrice *float64          `json:"discountPrice"`
		Stock         int               `json:"stock"`
		Gallery       []string          `json:"gallery"`
		Variations    map[string]string `json:"variations"`
		Shipping      struct {
			IsFree bool `json:"isFree"`
		} `json:"shipping"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// Clean out empty gallery strings
	var cleanGallery []string
	for _, img := range req.Gallery {
		if img != "" {
			cleanGallery = append(cleanGallery, img)
		}
	}

	galleryJSON, _ := json.Marshal(cleanGallery)
	varsJSON, _ := json.Marshal(req.Variations)

	query := `
		UPDATE products 
		SET name = $1, description = $2, price = $3, discount_price = $4, 
		    stock = $5, gallery = $6, variations = $7, is_free_shipping = $8, updated_at = CURRENT_TIMESTAMP
		WHERE id = $9
	`
	_, err = app.DB.Exec(ctx, query, req.Name, req.Description, req.Price, req.DiscountPrice, req.Stock, galleryJSON, varsJSON, req.Shipping.IsFree, productID)

	if err != nil {
		app.Logger.Error("Failed to update product", "error", err)
		http.Error(w, "Failed to update product", http.StatusInternalServerError)
		return
	}

	app.WriteJSON(w, r, http.StatusOK, map[string]string{"status": "success"})
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
