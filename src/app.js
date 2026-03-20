// 1. Import third-party libraries
import Alpine from "alpinejs";

// 2. Import your AUTO-GENERATED icons!
import { createIcons, myIcons } from "./generated-icons.js";

// 3. Wrap createIcons to use your optimized list
window.lucide = {
  createIcons: (options = {}) => {
    createIcons({
      icons: myIcons,
      ...options,
    });
  },
};

// 4. Expose Alpine and WebAuthn to the global window object
window.Alpine = Alpine;

// 5. Import your custom static files
import "./theme.js";

// 6. Initialize Alpine
Alpine.start();
