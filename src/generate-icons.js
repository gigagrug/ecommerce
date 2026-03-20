import fs from "fs";
import path from "path";

// Helper function to recursively find all HTML files
function findHtmlFiles(dir, fileList = []) {
  const files = fs.readdirSync(dir);
  for (const file of files) {
    if (file === "node_modules" || file.startsWith(".")) continue;

    const filePath = path.join(dir, file);
    if (fs.statSync(filePath).isDirectory()) {
      findHtmlFiles(filePath, fileList);
    } else if (file.endsWith(".html")) {
      fileList.push(filePath);
    }
  }
  return fileList;
}

function generateIcons() {
  const manualIcons = ["folder-plus", "save", "laptop", "shirt", "house", "dumbbell", "gamepad-2", "book", "shield-alert", "file-edit", "monitor", "smartphone", "layout-dashboard"];

  const usedIcons = new Set(manualIcons);
  const regex = /data-lucide=["']([a-z0-9-]+)["']/g;

  // 1. Get all HTML files
  const htmlFiles = findHtmlFiles(".");

  // 2. Scan them for icons
  for (const file of htmlFiles) {
    const content = fs.readFileSync(file, "utf-8");
    let match;
    while ((match = regex.exec(content)) !== null) {
      usedIcons.add(match[1]);
    }
  }

  // 3. Convert to PascalCase
  const toPascalCase = (str) => {
    return str
      .split("-")
      .map((word) => word.charAt(0).toUpperCase() + word.slice(1))
      .join("");
  };

  const importNames = Array.from(usedIcons).map(toPascalCase).sort();

  // 4. Generate the code
  const generatedCode = `// ⚠️ AUTO-GENERATED FILE - DO NOT EDIT MANUALLY
// Run 'npm run build' to update this list based on your HTML files.

import { 
    createIcons, 
    ${importNames.join(",\n    ")} 
} from 'lucide';

export const myIcons = {
    ${importNames.join(",\n    ")}
};

export { createIcons };
`;

  // 5. Write to src folder
  fs.writeFileSync(path.join("src", "generated-icons.js"), generatedCode);
  console.log(`✅ Bundled ${manualIcons.length} manual and ${importNames.length - manualIcons.length} auto-detected Lucide icons via Node.js!`);
}

generateIcons();
