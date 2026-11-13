// Applies the saved theme before first paint to avoid a flash. Loaded in
// <head> without defer.
(function () {
  var root = document.documentElement;
  root.classList.add("js");
  try {
    var t = localStorage.getItem("theme");
    if (t === "light" || t === "dark") root.setAttribute("data-theme", t);
  } catch (e) { /* storage unavailable: follow the system theme */ }
})();
