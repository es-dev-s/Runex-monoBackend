package stackmeta

import "strings"

func Color(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "go", "golang":
		return "#00ADD8"
	case "typescript":
		return "#3178c6"
	case "javascript":
		return "#f1e05a"
	case "python":
		return "#3572A5"
	case "html":
		return "#e34c26"
	case "css":
		return "#563d7c"
	case "rust":
		return "#dea584"
	case "tauri":
		return "#ffc131"
	case "java":
		return "#b07219"
	case "c":
		return "#555555"
	case "c++", "cpp":
		return "#f34b7d"
	case "php":
		return "#4F5D95"
	case "ruby":
		return "#701516"
	case "nextjs", "next.js":
		return "#000000"
	case "react":
		return "#61dafb"
	case "fastapi":
		return "#009688"
	case "nodejs", "node.js", "express":
		return "#339933"
	case "postgresql", "postgres":
		return "#336791"
	case "redis":
		return "#dc382d"
	case "vite":
		return "#646cff"
	case "sqlite":
		return "#003b57"
	default:
		return "#8b949e"
	}
}

func Keep(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "ignore list", "dotenv", "dockerfile", "makefile", "go checksums", "go module", "pip requirements", "svg", "shell", "docker", "procfile", "static":
		return false
	default:
		return true
	}
}

func Normalize(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "tsx":
		return "TypeScript"
	case "jsx":
		return "JavaScript"
	default:
		return Label(name)
	}
}

func Label(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "nextjs":
		return "Next.js"
	case "nodejs":
		return "Node.js"
	case "net/http":
		return "Go HTTP"
	case "html":
		return "HTML"
	case "css":
		return "CSS"
	case "javascript":
		return "JavaScript"
	case "typescript":
		return "TypeScript"
	case "python":
		return "Python"
	case "go", "golang":
		return "Go"
	case "fastapi":
		return "FastAPI"
	case "postgresql", "postgres":
		return "PostgreSQL"
	case "redis":
		return "Redis"
	case "express":
		return "Express"
	case "vite":
		return "Vite"
	case "rust":
		return "Rust"
	case "tauri":
		return "Tauri"
	case "static":
		return "Static"
	default:
		return strings.TrimSpace(name)
	}
}
