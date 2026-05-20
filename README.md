# Go E-Commerce Platform
A high-performance, containerized e-commerce backend API and storefront engine. Designed with a focus on system optimization, secure payment processing, and automated cloud deployments, this platform handles the full lifecycle of an online storefront—from inventory management and dynamic product variations to secure checkout and role-based administration.

Built as a lead developer project in March 2026, this system avoids heavy web frameworks in favor of Go's robust standard library and raw SQL, ensuring maximum performance and maintainability.

## 🛠 Technical Stack
- Language: Go (Golang) leveraging Go 1.22+ standard library routing.
- Database: PostgreSQL (pgx/v5 connection pooling) with complex relational schemas.
- Authentication: Custom session-based auth, Bcrypt password hashing, and OTP email verification via Resend.
- Payments: Stripe API (stripe-go/v84) integration for secure Payment Intents.
- Infrastructure: Docker, Docker Compose, Hetzner Cloud.
- CI/CD: GitHub Actions, GitHub Container Registry (GHCR).

## ✨ Key Features & Architecture
### 🔐 Custom Authentication & Security
- Session Management: Hand-rolled, secure, HTTP-only cookie session management with SHA-256 token hashing and database-backed expiration validation.
- OTP Verification: Email ownership verification via one-time passwords (OTP) before granting system access, preventing ghost accounts.
- Concurrent Session Control: Tracks OS, browser, and IP location data, allowing users to view and revoke active sessions remotely.

## 🛒 Product & Order Management
- Dynamic Inventory: Supports complex product groups with dynamic variations (e.g., size, color), discount pricing logic, and stock management.
- Cart & Checkout Pipeline: Stateless cart management via URL-encoded cookies, dynamically calculating sub-totals, real-time tax (8%), and dynamic shipping options based on product flags.
- Order Tracking: Generates timestamped order timelines and maps Stripe payment states directly to the database.

## 🏢 Role-Based Access Control (RBAC)
- Admin Dashboard: Protected middleware routes enforcing specific employee access levels (Admin, products, orders, support).
- Support Ticketing: Integrated customer support ticketing system allowing two-way communication between users and authorized staff.

## 🚀 CI/CD & DevOps Pipeline
- Automated Builds: GitHub Actions workflow (publish-image.yaml) extracts metadata and automatically builds/pushes multi-architecture Docker images to ghcr.io upon branch updates.
- Zero-Downtime Deployment: The deploy.yaml pipeline dynamically provisions a temporary Hetzner cloud firewall for the GitHub Runner's IP, securely executes a remote Docker Compose pull/up sequence via SSH, verifies container health, and safely tears down the firewall post-deployment.
