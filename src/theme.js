setInitialTheme();
function setInitialTheme() {
  const savedTheme = localStorage.getItem("theme") || (window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
  document.documentElement.className = "";
  document.documentElement.classList.add(savedTheme);
}
window.theme = function () {
  const isDark = document.documentElement.classList.contains("dark");
  const newTheme = isDark ? "light" : "dark";
  document.documentElement.classList.remove(isDark ? "dark" : "light");
  document.documentElement.classList.add(newTheme);
  localStorage.setItem("theme", newTheme);
};
