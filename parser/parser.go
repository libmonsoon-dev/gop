/*
 * Copyright (c) 2021 The GoPlus Authors (goplus.org). All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package parser implements a parser for Go+ source files. Input may be
// provided in a variety of forms (see the various Parse* functions); the
// output is an abstract syntax tree (AST) representing the Go source. The
// parser is invoked through one of the Parse* functions.
//
// The parser accepts a larger language than is syntactically permitted by
// the Go+ spec, for simplicity, and for improved robustness in the presence
// of syntax errors. For instance, in method declarations, the receiver is
// treated like an ordinary parameter list and thus may contain multiple
// entries where the spec permits exactly one. Consequently, the corresponding
// field in the AST (ast.FuncDecl.Recv) field is not restricted to one entry.
//
package parser

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/goplus/gop/ast"
	"github.com/goplus/gop/scanner"
	"github.com/goplus/gop/token"
	"github.com/qiniu/x/log"
)

const (
	msgTupleNotSupported = "tuple is not supported"
)

// The parser structure holds the parser's internal state.
type parser struct {
	file    *token.File
	errors  scanner.ErrorList
	scanner scanner.Scanner

	// Tracing/debugging
	mode         Mode // parsing mode
	trace        bool // == (mode & Trace != 0)
	noEntrypoint bool // no entrypoint func
	indent       int  // indentation used for tracing output

	// Comments
	comments    []*ast.CommentGroup
	leadComment *ast.CommentGroup // last lead comment
	lineComment *ast.CommentGroup // last line comment

	// Next token
	pos token.Pos   // token position
	tok token.Token // one token look-ahead
	lit string      // token literal
	old struct {
		pos token.Pos
		tok token.Token
		lit string
	}

	// Error recovery
	// (used to limit the number of calls to parser.advance
	// w/o making scanning progress - avoids potential endless
	// loops across multiple parser functions during error recovery)
	syncPos token.Pos // last synchronization position
	syncCnt int       // number of parser.advance calls without progress

	// Non-syntactic parser control
	exprLev int  // < 0: in control clause, >= 0: in expression
	inRHS   bool // if set, the parser is parsing a rhs expression

	// Ordinary identifier scopes
	pkgScope   *ast.Scope        // pkgScope.Outer == nil
	topScope   *ast.Scope        // top-most scope; may be pkgScope
	unresolved []*ast.Ident      // unresolved identifiers
	imports    []*ast.ImportSpec // list of imports

	// Label scopes
	// (maintained by open/close LabelScope)
	labelScope  *ast.Scope     // label scope for current function
	targetStack [][]*ast.Ident // stack of unresolved labels
}

func (p *parser) init(fset *token.FileSet, filename string, src []byte, mode Mode) {
	p.file = fset.AddFile(filename, -1, len(src))
	var m scanner.Mode
	if mode&ParseComments != 0 {
		m = scanner.ScanComments
	}
	eh := func(pos token.Position, msg string) { p.errors.Add(pos, msg) }
	p.scanner.Init(p.file, src, eh, m)

	p.mode = mode
	p.trace = mode&Trace != 0 // for convenience (p.trace is used frequently)

	p.next()
}

// ----------------------------------------------------------------------------
// Scoping support

func (p *parser) openScope() {
	p.topScope = ast.NewScope(p.topScope)
}

func (p *parser) closeScope() {
	p.topScope = p.topScope.Outer
}

func (p *parser) openLabelScope() {
	p.labelScope = ast.NewScope(p.labelScope)
	p.targetStack = append(p.targetStack, nil)
}

func (p *parser) closeLabelScope() {
	// resolve labels
	n := len(p.targetStack) - 1
	scope := p.labelScope
	for _, ident := range p.targetStack[n] {
		ident.Obj = scope.Lookup(ident.Name)
		if ident.Obj == nil && p.mode&DeclarationErrors != 0 {
			p.error(ident.Pos(), fmt.Sprintf("label %s undefined", ident.Name))
		}
	}
	// pop label scope
	p.targetStack = p.targetStack[0:n]
	p.labelScope = p.labelScope.Outer
}

func (p *parser) declare(decl, data interface{}, scope *ast.Scope, kind ast.ObjKind, idents ...*ast.Ident) {
	for _, ident := range idents {
		assert(ident.Obj == nil, "identifier already declared or resolved")
		obj := ast.NewObj(kind, ident.Name)
		// remember the corresponding declaration for redeclaration
		// errors and global variable resolution/typechecking phase
		obj.Decl = decl
		obj.Data = data
		ident.Obj = obj
		if ident.Name != "_" {
			if alt := scope.Insert(obj); alt != nil && p.mode&DeclarationErrors != 0 {
				prevDecl := ""
				if pos := alt.Pos(); pos.IsValid() {
					prevDecl = fmt.Sprintf("\n\tprevious declaration at %s", p.file.Position(pos))
				}
				p.error(ident.Pos(), fmt.Sprintf("%s redeclared in this block%s", ident.Name, prevDecl))
			}
		}
	}
}

func (p *parser) shortVarDecl(decl *ast.AssignStmt, list []ast.Expr) {
	// Go spec: A short variable declaration may redeclare variables
	// provided they were originally declared in the same block with
	// the same type, and at least one of the non-blank variables is new.
	n := 0 // number of new variables
	for _, x := range list {
		if ident, isIdent := x.(*ast.Ident); isIdent {
			assert(ident.Obj == nil, "identifier already declared or resolved")
			obj := ast.NewObj(ast.Var, ident.Name)
			// remember corresponding assignment for other tools
			obj.Decl = decl
			ident.Obj = obj
			if ident.Name != "_" {
				if alt := p.topScope.Insert(obj); alt != nil {
					ident.Obj = alt // redeclaration
				} else {
					n++ // new declaration
				}
			}
		} else {
			p.errorExpected(x.Pos(), "identifier on left side of :=", 2)
		}
	}
	if n == 0 && p.mode&DeclarationErrors != 0 {
		p.error(list[0].Pos(), "no new variables on left side of :=")
	}
}

// The unresolved object is a sentinel to mark identifiers that have been added
// to the list of unresolved identifiers. The sentinel is only used for verifying
// internal consistency.
var unresolved = new(ast.Object)

// If x is an identifier, tryResolve attempts to resolve x by looking up
// the object it denotes. If no object is found and collectUnresolved is
// set, x is marked as unresolved and collected in the list of unresolved
// identifiers.
//
func (p *parser) tryResolve(x ast.Expr, collectUnresolved bool) {
	// nothing to do if x is not an identifier or the blank identifier
	ident, _ := x.(*ast.Ident)
	if ident == nil {
		return
	}
	assert(ident.Obj == nil, "identifier already declared or resolved")
	if ident.Name == "_" {
		return
	}
	// try to resolve the identifier
	for s := p.topScope; s != nil; s = s.Outer {
		if obj := s.Lookup(ident.Name); obj != nil {
			ident.Obj = obj
			return
		}
	}
	// all local scopes are known, so any unresolved identifier
	// must be found either in the file scope, package scope
	// (perhaps in another file), or universe scope --- collect
	// them so that they can be resolved later
	if collectUnresolved {
		ident.Obj = unresolved
		p.unresolved = append(p.unresolved, ident)
	}
}

func (p *parser) resolve(x ast.Expr) {
	p.tryResolve(x, true)
}

// ----------------------------------------------------------------------------
// Parsing support

func (p *parser) printTrace(a ...interface{}) {
	const dots = ". . . . . . . . . . . . . . . . . . . . . . . . . . . . . . . . "
	const n = len(dots)
	pos := p.file.Position(p.pos)
	fmt.Printf("%5d:%3d: ", pos.Line, pos.Column)
	i := 2 * p.indent
	for i > n {
		fmt.Print(dots)
		i -= n
	}
	// i <= n
	fmt.Print(dots[0:i])
	fmt.Println(a...)
}

func trace(p *parser, msg string) *parser {
	p.printTrace(msg, "(")
	p.indent++
	return p
}

// Usage pattern: defer un(trace(p, "..."))
func un(p *parser) {
	p.indent--
	p.printTrace(")")
}

func (p *parser) unget(pos token.Pos, tok token.Token, lit string) {
	p.old.pos, p.old.tok, p.old.lit = p.pos, p.tok, p.lit
	p.pos, p.tok, p.lit = pos, tok, lit
}

// Advance to the next token.
func (p *parser) next0() {
	if p.old.pos != 0 { // Go+: support unget
		p.pos, p.tok, p.lit = p.old.pos, p.old.tok, p.old.lit
		p.old.pos = 0
		return
	}

	// Because of one-token look-ahead, print the previous token
	// when tracing as it provides a more readable output. The
	// very first token (!p.pos.IsValid()) is not initialized
	// (it is token.ILLEGAL), so don't print it .
	if p.trace && p.pos.IsValid() {
		s := p.tok.String()
		switch {
		case p.tok.IsLiteral():
			p.printTrace(s, p.lit)
		case p.tok.IsOperator(), p.tok.IsKeyword():
			p.printTrace("\"" + s + "\"")
		default:
			p.printTrace(s)
		}
	}

	p.pos, p.tok, p.lit = p.scanner.Scan()
}

// Consume a comment and return it and the line on which it ends.
func (p *parser) consumeComment() (comment *ast.Comment, endline int) {
	// /*-style comments may end on a different line than where they start.
	// Scan the comment for '\n' chars and adjust endline accordingly.
	endline = p.file.Line(p.pos)
	if p.lit[1] == '*' {
		// don't use range here - no need to decode Unicode code points
		for i := 0; i < len(p.lit); i++ {
			if p.lit[i] == '\n' {
				endline++
			}
		}
	}

	comment = &ast.Comment{Slash: p.pos, Text: p.lit}
	p.next0()

	return
}

// Consume a group of adjacent comments, add it to the parser's
// comments list, and return it together with the line at which
// the last comment in the group ends. A non-comment token or n
// empty lines terminate a comment group.
//
func (p *parser) consumeCommentGroup(n int) (comments *ast.CommentGroup, endline int) {
	var list []*ast.Comment
	endline = p.file.Line(p.pos)
	for p.tok == token.COMMENT && p.file.Line(p.pos) <= endline+n {
		var comment *ast.Comment
		comment, endline = p.consumeComment()
		list = append(list, comment)
	}

	// add comment group to the comments list
	comments = &ast.CommentGroup{List: list}
	p.comments = append(p.comments, comments)

	return
}

// Advance to the next non-comment token. In the process, collect
// any comment groups encountered, and remember the last lead and
// line comments.
//
// A lead comment is a comment group that starts and ends in a
// line without any other tokens and that is followed by a non-comment
// token on the line immediately after the comment group.
//
// A line comment is a comment group that follows a non-comment
// token on the same line, and that has no tokens after it on the line
// where it ends.
//
// Lead and line comments may be considered documentation that is
// stored in the AST.
//
func (p *parser) next() {
	p.leadComment = nil
	p.lineComment = nil
	prev := p.pos
	p.next0()

	if p.tok == token.COMMENT {
		var comment *ast.CommentGroup
		var endline int

		if p.file.Line(p.pos) == p.file.Line(prev) {
			// The comment is on same line as the previous token; it
			// cannot be a lead comment but may be a line comment.
			comment, endline = p.consumeCommentGroup(0)
			if p.file.Line(p.pos) != endline || p.tok == token.EOF {
				// The next token is on a different line, thus
				// the last comment group is a line comment.
				p.lineComment = comment
			}
		}

		// consume successor comments, if any
		endline = -1
		for p.tok == token.COMMENT {
			comment, endline = p.consumeCommentGroup(1)
		}

		if endline+1 == p.file.Line(p.pos) {
			// The next token is following on the line immediately after the
			// comment group, thus the last comment group is a lead comment.
			p.leadComment = comment
		}
	}
}

// A bailout panic is raised to indicate early termination.
type bailout struct{}

func (p *parser) error(pos token.Pos, msg string) {
	epos := p.file.Position(pos)

	// If AllErrors is not set, discard errors reported on the same line
	// as the last recorded error and stop parsing if there are more than
	// 10 errors.
	if p.mode&AllErrors == 0 {
		n := len(p.errors)
		if n > 0 && p.errors[n-1].Pos.Line == epos.Line {
			return // discard - likely a spurious error
		}
		if n > 10 {
			panic(bailout{})
		}
	}

	p.errors.Add(epos, msg)
}

func (p *parser) errorExpected(pos token.Pos, msg string, calldepth int) {
	msg = "expected " + msg
	if pos == p.pos {
		// the error happened at the current position;
		// make the error message more specific
		switch {
		case p.tok == token.SEMICOLON && p.lit == "\n":
			msg += ", found newline"
		case p.tok.IsLiteral():
			// print 123 rather than 'INT', etc.
			msg += ", found " + p.lit
		default:
			msg += ", found '" + p.tok.String() + "'"
		}
	}
	if debugParseError {
		log.Std.Output("", log.Linfo, calldepth, msg)
	}
	p.error(pos, msg)
}

func (p *parser) expect(tok token.Token) token.Pos {
	pos := p.pos
	if p.tok != tok {
		p.errorExpected(pos, "'"+tok.String()+"'", 3)
	}
	p.next() // make progress
	return pos
}

// expect2 is like expect, but it returns an invalid position
// if the expected token is not found.
func (p *parser) expect2(tok token.Token) (pos token.Pos) {
	if p.tok == tok {
		pos = p.pos
	} else {
		p.errorExpected(p.pos, "'"+tok.String()+"'", 3)
	}
	p.next() // make progress
	return
}

// expectClosing is like expect but provides a better error message
// for the common case of a missing comma before a newline.
//
func (p *parser) expectClosing(tok token.Token, context string) token.Pos {
	if p.tok != tok && p.tok == token.SEMICOLON && p.lit == "\n" {
		p.error(p.pos, "missing ',' before newline in "+context)
		p.next()
	}
	return p.expect(tok)
}

func (p *parser) expectSemi() {
	// semicolon is optional before a closing ')' or '}'
	if p.tok != token.RPAREN && p.tok != token.RBRACE {
		switch p.tok {
		case token.COMMA:
			// permit a ',' instead of a ';' but complain
			p.errorExpected(p.pos, "';'", 3)
			fallthrough
		case token.SEMICOLON:
			p.next()
		default:
			p.errorExpected(p.pos, "';'", 3)
			p.advance(stmtStart)
		}
	}
}

func (p *parser) atComma(context string, follow token.Token) bool {
	if p.tok == token.COMMA {
		return true
	}
	if p.tok != follow {
		msg := "missing ','"
		if p.tok == token.SEMICOLON && p.lit == "\n" {
			msg += " before newline"
		}
		msgctx := msg + " in " + context
		p.error(p.pos, msgctx)
		if debugParseError {
			log.Std.Output("", log.Linfo, 2, msgctx)
		}
		return true // "insert" comma and continue
	}
	return false
}

func assert(cond bool, msg string) {
	if !cond {
		panic("go/parser internal error: " + msg)
	}
}

// advance consumes tokens until the current token p.tok
// is in the 'to' set, or token.EOF. For error recovery.
func (p *parser) advance(to map[token.Token]bool) {
	for ; p.tok != token.EOF; p.next() {
		if to[p.tok] {
			// Return only if parser made some progress since last
			// sync or if it has not reached 10 advance calls without
			// progress. Otherwise consume at least one token to
			// avoid an endless parser loop (it is possible that
			// both parseOperand and parseStmt call advance and
			// correctly do not advance, thus the need for the
			// invocation limit p.syncCnt).
			if p.pos == p.syncPos && p.syncCnt < 10 {
				p.syncCnt++
				return
			}
			if p.pos > p.syncPos {
				p.syncPos = p.pos
				p.syncCnt = 0
				return
			}
			// Reaching here indicates a parser bug, likely an
			// incorrect token list in this function, but it only
			// leads to skipping of possibly correct code if a
			// previous error is present, and thus is preferred
			// over a non-terminating parse.
		}
	}
}

var stmtStart = map[token.Token]bool{
	token.BREAK:       true,
	token.CONST:       true,
	token.CONTINUE:    true,
	token.DEFER:       true,
	token.FALLTHROUGH: true,
	token.FOR:         true,
	token.GO:          true,
	token.GOTO:        true,
	token.IF:          true,
	token.RETURN:      true,
	token.SELECT:      true,
	token.SWITCH:      true,
	token.TYPE:        true,
	token.VAR:         true,
}

var declStart = map[token.Token]bool{
	token.CONST: true,
	token.TYPE:  true,
	token.VAR:   true,
}

var exprEnd = map[token.Token]bool{
	token.COMMA:     true,
	token.COLON:     true,
	token.SEMICOLON: true,
	token.RPAREN:    true,
	token.RBRACK:    true,
	token.RBRACE:    true,
}

// safePos returns a valid file position for a given position: If pos
// is valid to begin with, safePos returns pos. If pos is out-of-range,
// safePos returns the EOF position.
//
// This is hack to work around "artificial" end positions in the AST which
// are computed by adding 1 to (presumably valid) token positions. If the
// token positions are invalid due to parse errors, the resulting end position
// may be past the file's EOF position, which would lead to panics if used
// later on.
//
func (p *parser) safePos(pos token.Pos) (res token.Pos) {
	defer func() {
		if recover() != nil {
			res = token.Pos(p.file.Base() + p.file.Size()) // EOF position
		}
	}()
	_ = p.file.Offset(pos) // trigger a panic if position is out-of-range
	return pos
}

// ----------------------------------------------------------------------------
// Identifiers

func (p *parser) parseIdentOrOp() (*ast.Ident, bool) { // function Name
	if int(p.tok) < len(overloadOps) {
		flags := overloadOps[p.tok]
		if flags != 0 {
			pos := p.pos
			tok := p.tok
			p.next()
			if debugParseOutput {
				log.Printf("ast.Ident{Tok: %v}\n", tok)
			}
			return &ast.Ident{NamePos: pos, Name: tok.String()}, true
		}
	}
	return p.parseIdent(), false
}

const (
	opUnary = 1 << iota
	opBinary
	opAssign
	opAssignOp
	opIncDec
)

var overloadOps = [...]byte{
	token.ADD: opBinary,           // +
	token.SUB: opBinary | opUnary, // -
	token.MUL: opBinary | opUnary, // *
	token.QUO: opBinary,           // /
	token.REM: opBinary,           // %

	token.AND:     opBinary, // &
	token.OR:      opBinary, // |
	token.XOR:     opBinary, // ^
	token.SHL:     opBinary, // <<
	token.SHR:     opBinary, // >>
	token.AND_NOT: opBinary, // &^

	token.ADD_ASSIGN: opAssignOp, // +=
	token.SUB_ASSIGN: opAssignOp, // -=
	token.MUL_ASSIGN: opAssignOp, // *=
	token.QUO_ASSIGN: opAssignOp, // /=
	token.REM_ASSIGN: opAssignOp, // %=

	token.AND_ASSIGN:     opAssignOp, // &=
	token.OR_ASSIGN:      opAssignOp, // |=
	token.XOR_ASSIGN:     opAssignOp, // ^=
	token.SHL_ASSIGN:     opAssignOp, // <<=
	token.SHR_ASSIGN:     opAssignOp, // >>=
	token.AND_NOT_ASSIGN: opAssignOp, // &^=

	token.ASSIGN: opAssign, // =

	token.INC: opIncDec, // ++
	token.DEC: opIncDec, // --

	token.EQL:  opBinary, // ==
	token.LSS:  opBinary, // <
	token.GTR:  opBinary, // >
	token.NEQ:  opBinary, // !=
	token.LEQ:  opBinary, // <=
	token.GEQ:  opBinary, // >=
	token.LAND: opBinary, // &&
	token.LOR:  opBinary, // ||
	token.NOT:  opUnary,  // !

	token.ARROW: opBinary | opUnary, // <-
}

func (p *parser) parseIdent() *ast.Ident {
	pos := p.pos
	name := "_"
	if p.tok == token.IDENT {
		name = p.lit
		p.next()
	} else {
		p.expect(token.IDENT) // use expect() error handling
	}
	if debugParseOutput {
		log.Printf("ast.Ident{Name: %v}\n", name)
	}
	return &ast.Ident{NamePos: pos, Name: name}
}

func (p *parser) parseIdentList() (list []*ast.Ident) {
	if p.trace {
		defer un(trace(p, "IdentList"))
	}

	list = append(list, p.parseIdent())
	for p.tok == token.COMMA {
		p.next()
		list = append(list, p.parseIdent())
	}
	return
}

// ----------------------------------------------------------------------------
// Common productions

// If lhs is set, result list elements which are identifiers are not resolved.
func (p *parser) parseExprList(lhs, allowCmd bool) (list []ast.Expr) {
	if p.trace {
		defer un(trace(p, "ExpressionList"))
	}

	list = append(list, p.checkExpr(p.parseExpr(lhs, allowCmd, false)))
	for p.tok == token.COMMA {
		p.next()
		list = append(list, p.checkExpr(p.parseExpr(lhs, false, false)))
	}
	return
}

func (p *parser) parseLHSList(allowCmd bool) []ast.Expr {
	old := p.inRHS
	p.inRHS = false
	list := p.parseExprList(true, allowCmd)
	switch p.tok {
	case token.DEFINE:
		// lhs of a short variable declaration
		// but doesn't enter scope until later:
		// caller must call p.shortVarDecl(p.makeIdentList(list))
		// at appropriate time.
	case token.COLON:
		// lhs of a label declaration or a communication clause of a select
		// statement (parseLhsList is not called when parsing the case clause
		// of a switch statement):
		// - labels are declared by the caller of parseLhsList
		// - for communication clauses, if there is a stand-alone identifier
		//   followed by a colon, we have a syntax error; there is no need
		//   to resolve the identifier in that case
	default:
		// identifiers must be declared elsewhere
		for _, x := range list {
			p.resolve(x)
		}
	}
	p.inRHS = old
	return list
}

func (p *parser) parseRHSList() []ast.Expr {
	old := p.inRHS
	p.inRHS = true
	list := p.parseExprList(false, false)
	p.inRHS = old
	return list
}

// ----------------------------------------------------------------------------
// Types

func (p *parser) parseType() ast.Expr {
	if p.trace {
		defer un(trace(p, "Type"))
	}

	typ := p.tryType()

	if typ == nil {
		pos := p.pos
		p.errorExpected(pos, "type", 2)
		p.advance(exprEnd)
		return &ast.BadExpr{From: pos, To: p.pos}
	}

	return typ
}

// If the result is an identifier, it is not resolved.
func (p *parser) parseTypeName() ast.Expr {
	if p.trace {
		defer un(trace(p, "TypeName"))
	}

	ident := p.parseIdent()
	// don't resolve ident yet - it may be a parameter or field name

	if p.tok == token.PERIOD {
		// ident is a package name
		p.next()
		p.resolve(ident)
		sel := p.parseIdent()
		return &ast.SelectorExpr{X: ident, Sel: sel}
	}

	return ident
}

const (
	stateArrayTypeOrSliceLit = iota
	stateTypeOrSliceOp
	stateType
)

const (
	resultNone      = 0
	resultArrayType = 1 << iota
	resultSliceLit
	resultSliceOp
	resultComprehensionExpr
	resultParenType
	resultType
	resultIdent     // expr or type
	resultExprFlags = resultSliceLit | resultSliceOp | resultComprehensionExpr
	resultTypeFlags = resultArrayType | resultParenType | resultType
)

// state = stateArrayTypeOrSliceLit | stateTypeOrSliceOp | stateType | ...
func (p *parser) parseArrayTypeOrSliceLit(state int, slice ast.Expr) (expr ast.Expr, result int) {
	if p.trace {
		defer un(trace(p, "ArrayType"))
	}

	lbrack := p.expect(token.LBRACK)
	p.exprLev++
	var len ast.Expr
	// always permit ellipsis for more fault-tolerant parsing
	if p.tok == token.ELLIPSIS {
		len = &ast.Ellipsis{Ellipsis: p.pos}
		p.next()
	} else if p.tok != token.RBRACK {
		len = p.parseRHS()
		switch state {
		case stateArrayTypeOrSliceLit:
			switch p.tok {
			case token.COMMA: // [a, b, c, d ...]
				sliceLit := p.parseSliceLit(lbrack, len)
				p.exprLev--
				return sliceLit, resultSliceLit
			case token.FOR: // [expr for k, v <- container if cond]
				phrases := p.parseForPhrases()
				p.exprLev--
				rbrack := p.expect(token.RBRACK)
				if debugParseOutput {
					log.Printf("ast.ComprehensionExpr{Tok: [, Elt: %v, Fors: %v}\n", len, phrases)
				}
				return &ast.ComprehensionExpr{
					Lpos: lbrack, Tok: token.LBRACK, Elt: len,
					Fors: phrases, Rpos: rbrack,
				}, resultComprehensionExpr
			}
		case stateTypeOrSliceOp:
			switch p.tok {
			case token.COLON: // slice[i:j:k]
				return p.parseIndexOrSliceContinue(slice, lbrack, len), resultSliceOp
			}
		}
	}
	p.exprLev--
	rbrack := p.expect(token.RBRACK)

	var elt ast.Expr
	switch state {
	case stateType:
		elt = p.parseType()
	case stateArrayTypeOrSliceLit:
		sliceLit := newSliceLit(lbrack, rbrack, len)
		elt, result = p.tryIdentOrType(stateTypeOrSliceOp, sliceLit)
		switch result {
		case resultNone:
			if debugParseOutput {
				log.Printf("ast.SliceLit{Elts: %v}\n", sliceLit.Elts)
			}
			return sliceLit, resultSliceLit
		case resultSliceOp:
			return elt, resultSliceOp
		}
		p.resolve(elt)
	case stateTypeOrSliceOp:
		elt = p.tryType()
		if elt == nil {
			if len == nil {
				log.Panicln("TODO: expect slice index")
			}
			if debugParseOutput {
				log.Printf("ast.IndexExpr{X: %v, Index: %v}\n", slice, len)
			}
			return &ast.IndexExpr{X: slice, Index: len}, resultSliceOp
		}
	default:
		panic("parseArrayTypeOrSliceLit: unexpected state")
	}

	if debugParseOutput {
		log.Printf("ast.ArrayType{Len: %v, Elt: %v}\n", len, elt)
	}
	return &ast.ArrayType{Lbrack: lbrack, Len: len, Elt: elt}, resultArrayType
}

func (p *parser) parseSliceLit(lbrack token.Pos, len ast.Expr) ast.Expr {
	elts := make([]ast.Expr, 1, 8)
	elts[0] = len
	for p.tok == token.COMMA {
		p.next()
		if p.tok != token.RBRACK {
			elt := p.parseRHS()
			elts = append(elts, elt)
		}
	}
	rbrack := p.expect(token.RBRACK)

	if debugParseOutput {
		log.Printf("ast.SliceLit{Elts: %v}\n", elts)
	}
	return &ast.SliceLit{Lbrack: lbrack, Elts: elts, Rbrack: rbrack}
}

func newSliceLit(lbrack, rbrack token.Pos, len ast.Expr) *ast.SliceLit {
	var elts []ast.Expr
	if len != nil {
		elts = []ast.Expr{len}
	}
	return &ast.SliceLit{Lbrack: lbrack, Elts: elts, Rbrack: rbrack}
}

func (p *parser) makeIdentList(list []ast.Expr) []*ast.Ident {
	idents := make([]*ast.Ident, len(list))
	for i, x := range list {
		ident, isIdent := x.(*ast.Ident)
		if !isIdent {
			if _, isBad := x.(*ast.BadExpr); !isBad {
				// only report error if it's a new one
				p.errorExpected(x.Pos(), "identifier", 2)
			}
			ident = &ast.Ident{NamePos: x.Pos(), Name: "_"}
		}
		idents[i] = ident
	}
	return idents
}

func (p *parser) parseFieldDecl(scope *ast.Scope) *ast.Field {
	if p.trace {
		defer un(trace(p, "FieldDecl"))
	}

	doc := p.leadComment

	// 1st FieldDecl
	// A type name used as an anonymous field looks like a field identifier.
	var list []ast.Expr
	for {
		list = append(list, p.parseVarType(false))
		if p.tok != token.COMMA {
			break
		}
		p.next()
	}

	typ := p.tryVarType(false)

	// analyze case
	var idents []*ast.Ident
	if typ != nil {
		// IdentifierList Type
		idents = p.makeIdentList(list)
	} else {
		// ["*"] TypeName (AnonymousField)
		typ = list[0] // we always have at least one element
		if n := len(list); n > 1 {
			p.errorExpected(p.pos, "type", 2)
			typ = &ast.BadExpr{From: p.pos, To: p.pos}
		} else if !isTypeName(deref(typ)) {
			p.errorExpected(typ.Pos(), "anonymous field", 2)
			typ = &ast.BadExpr{From: typ.Pos(), To: p.safePos(typ.End())}
		}
	}

	// Tag
	var tag *ast.BasicLit
	if p.tok == token.STRING {
		tag = &ast.BasicLit{ValuePos: p.pos, Kind: p.tok, Value: p.lit}
		p.next()
	}

	p.expectSemi() // call before accessing p.linecomment

	field := &ast.Field{Doc: doc, Names: idents, Type: typ, Tag: tag, Comment: p.lineComment}
	p.declare(field, nil, scope, ast.Var, idents...)
	p.resolve(typ)

	return field
}

func (p *parser) parseStructType() *ast.StructType {
	if p.trace {
		defer un(trace(p, "StructType"))
	}

	pos := p.expect(token.STRUCT)
	lbrace := p.expect(token.LBRACE)
	scope := ast.NewScope(nil) // struct scope
	var list []*ast.Field
	for p.tok == token.IDENT || p.tok == token.MUL || p.tok == token.LPAREN {
		// a field declaration cannot start with a '(' but we accept
		// it here for more robust parsing and better error messages
		// (parseFieldDecl will check and complain if necessary)
		list = append(list, p.parseFieldDecl(scope))
	}
	rbrace := p.expect(token.RBRACE)

	return &ast.StructType{
		Struct: pos,
		Fields: &ast.FieldList{
			Opening: lbrace,
			List:    list,
			Closing: rbrace,
		},
	}
}

func (p *parser) parsePointerType() *ast.StarExpr {
	if p.trace {
		defer un(trace(p, "PointerType"))
	}

	star := p.expect(token.MUL)
	base := p.parseType()

	return &ast.StarExpr{Star: star, X: base}
}

// If the result is an identifier, it is not resolved.
func (p *parser) tryVarType(isParam bool) ast.Expr {
	if isParam && p.tok == token.ELLIPSIS {
		pos := p.pos
		p.next()
		typ, _ := p.tryIdentOrType(stateType, nil) // don't use parseType so we can provide better error message
		if typ != nil {
			p.resolve(typ)
		} else {
			p.error(pos, "'...' parameter is missing type")
			typ = &ast.BadExpr{From: pos, To: p.pos}
		}
		return &ast.Ellipsis{Ellipsis: pos, Elt: typ}
	}
	typ, _ := p.tryIdentOrType(stateType, nil)
	return typ
}

// If the result is an identifier, it is not resolved.
func (p *parser) parseVarType(isParam bool) ast.Expr {
	typ := p.tryVarType(isParam)
	if typ == nil {
		pos := p.pos
		p.errorExpected(pos, "type", 2)
		p.next() // make progress
		typ = &ast.BadExpr{From: pos, To: p.pos}
	}
	return typ
}

func (p *parser) parseParameterList(scope *ast.Scope, ellipsisOk bool) (params []*ast.Field) {
	if p.trace {
		defer un(trace(p, "ParameterList"))
	}

	// 1st ParameterDecl
	// A list of identifiers looks like a list of type names.
	var list []ast.Expr
	for {
		list = append(list, p.parseVarType(ellipsisOk))
		if p.tok != token.COMMA {
			break
		}
		p.next()
		if p.tok == token.RPAREN {
			break
		}
	}

	// analyze case
	if typ := p.tryVarType(ellipsisOk); typ != nil {
		// IdentifierList Type
		idents := p.makeIdentList(list)
		field := &ast.Field{Names: idents, Type: typ}
		params = append(params, field)
		// Go spec: The scope of an identifier denoting a function
		// parameter or result variable is the function body.
		p.declare(field, nil, scope, ast.Var, idents...)
		p.resolve(typ)
		if !p.atComma("parameter list", token.RPAREN) {
			return
		}
		p.next()
		for p.tok != token.RPAREN && p.tok != token.EOF {
			idents := p.parseIdentList()
			typ := p.parseVarType(ellipsisOk)
			field := &ast.Field{Names: idents, Type: typ}
			params = append(params, field)
			// Go spec: The scope of an identifier denoting a function
			// parameter or result variable is the function body.
			p.declare(field, nil, scope, ast.Var, idents...)
			p.resolve(typ)
			if !p.atComma("parameter list", token.RPAREN) {
				break
			}
			p.next()
		}
		return
	}

	// Type { "," Type } (anonymous parameters)
	params = make([]*ast.Field, len(list))
	for i, typ := range list {
		p.resolve(typ)
		params[i] = &ast.Field{Type: typ}
	}
	return
}

func (p *parser) parseParameters(scope *ast.Scope, ellipsisOk bool) *ast.FieldList {
	if p.trace {
		defer un(trace(p, "Parameters"))
	}

	var params []*ast.Field
	lparen := p.expect(token.LPAREN)
	if p.tok != token.RPAREN {
		params = p.parseParameterList(scope, ellipsisOk)
	}
	rparen := p.expect(token.RPAREN)

	return &ast.FieldList{Opening: lparen, List: params, Closing: rparen}
}

func (p *parser) parseResult(scope *ast.Scope) *ast.FieldList {
	if p.trace {
		defer un(trace(p, "Result"))
	}

	if p.tok == token.LPAREN {
		return p.parseParameters(scope, false)
	}

	typ := p.tryType()
	if typ != nil {
		list := make([]*ast.Field, 1)
		list[0] = &ast.Field{Type: typ}
		return &ast.FieldList{List: list}
	}

	return nil
}

func (p *parser) parseSignature(scope *ast.Scope) (params, results *ast.FieldList) {
	if p.trace {
		defer un(trace(p, "Signature"))
	}

	params = p.parseParameters(scope, true)
	results = p.parseResult(scope)

	return
}

func (p *parser) parseFuncType() (*ast.FuncType, *ast.Scope) {
	if p.trace {
		defer un(trace(p, "FuncType"))
	}

	pos := p.expect(token.FUNC)
	scope := ast.NewScope(p.topScope) // function scope
	params, results := p.parseSignature(scope)

	return &ast.FuncType{Func: pos, Params: params, Results: results}, scope
}

func (p *parser) parseMethodSpec(scope *ast.Scope) *ast.Field {
	if p.trace {
		defer un(trace(p, "MethodSpec"))
	}

	doc := p.leadComment
	var idents []*ast.Ident
	var typ ast.Expr
	x := p.parseTypeName()
	if ident, isIdent := x.(*ast.Ident); isIdent && p.tok == token.LPAREN {
		// method
		idents = []*ast.Ident{ident}
		scope := ast.NewScope(nil) // method scope
		params, results := p.parseSignature(scope)
		typ = &ast.FuncType{Func: token.NoPos, Params: params, Results: results}
	} else {
		// embedded interface
		typ = x
		p.resolve(typ)
	}
	p.expectSemi() // call before accessing p.linecomment

	spec := &ast.Field{Doc: doc, Names: idents, Type: typ, Comment: p.lineComment}
	p.declare(spec, nil, scope, ast.Fun, idents...)

	return spec
}

func (p *parser) parseInterfaceType() *ast.InterfaceType {
	if p.trace {
		defer un(trace(p, "InterfaceType"))
	}

	pos := p.expect(token.INTERFACE)
	lbrace := p.expect(token.LBRACE)
	scope := ast.NewScope(nil) // interface scope
	var list []*ast.Field
	for p.tok == token.IDENT {
		list = append(list, p.parseMethodSpec(scope))
	}
	rbrace := p.expect(token.RBRACE)

	return &ast.InterfaceType{
		Interface: pos,
		Methods: &ast.FieldList{
			Opening: lbrace,
			List:    list,
			Closing: rbrace,
		},
	}
}

func (p *parser) parseMapType() *ast.MapType {
	if p.trace {
		defer un(trace(p, "MapType"))
	}

	pos := p.expect(token.MAP)
	p.expect(token.LBRACK)
	key := p.parseType()
	p.expect(token.RBRACK)
	value := p.parseType()
	if debugParseOutput {
		log.Printf("ast.MapType{Key: %v, Value: %v}\n", key, value)
	}
	return &ast.MapType{Map: pos, Key: key, Value: value}
}

func (p *parser) parseChanType() *ast.ChanType {
	if p.trace {
		defer un(trace(p, "ChanType"))
	}

	pos := p.pos
	dir := ast.SEND | ast.RECV
	var arrow token.Pos
	if p.tok == token.CHAN {
		p.next()
		if p.tok == token.ARROW {
			arrow = p.pos
			p.next()
			dir = ast.SEND
		}
	} else {
		arrow = p.expect(token.ARROW)
		p.expect(token.CHAN)
		dir = ast.RECV
	}
	value := p.parseType()

	return &ast.ChanType{Begin: pos, Arrow: arrow, Dir: dir, Value: value}
}

// state = stateArrayTypeOrSliceLit | stateTypeOrSliceOp | stateType | ...
// If the result is an identifier, it is not resolved.
func (p *parser) tryIdentOrType(state int, len ast.Expr) (ast.Expr, int) {
	switch p.tok {
	case token.IDENT:
		return p.parseTypeName(), resultIdent
	case token.LBRACK:
		return p.parseArrayTypeOrSliceLit(state, len)
	case token.STRUCT:
		return p.parseStructType(), resultType
	case token.MUL:
		return p.parsePointerType(), resultType
	case token.FUNC:
		typ, _ := p.parseFuncType()
		return typ, resultType
	case token.INTERFACE:
		return p.parseInterfaceType(), resultType
	case token.MAP:
		return p.parseMapType(), resultType
	case token.CHAN, token.ARROW:
		return p.parseChanType(), resultType
	case token.LPAREN:
		lparen := p.pos
		p.next()
		typ := p.parseType()
		rparen := p.expect(token.RPAREN)
		return &ast.ParenExpr{Lparen: lparen, X: typ, Rparen: rparen}, resultParenType
	}

	// no type found
	return nil, resultNone
}

func (p *parser) tryType() ast.Expr {
	typ, _ := p.tryIdentOrType(stateType, nil)
	if typ != nil {
		p.resolve(typ)
	}
	return typ
}

// ----------------------------------------------------------------------------
// Blocks

func (p *parser) parseStmtList() (list []ast.Stmt) {
	if p.trace {
		defer un(trace(p, "StatementList"))
	}

	for p.tok != token.CASE && p.tok != token.DEFAULT && p.tok != token.RBRACE && p.tok != token.EOF {
		list = append(list, p.parseStmt(true))
	}

	return
}

func (p *parser) parseBody(scope *ast.Scope) *ast.BlockStmt {
	if p.trace {
		defer un(trace(p, "Body"))
	}

	lbrace := p.expect(token.LBRACE)
	p.topScope = scope // open function scope
	p.openLabelScope()
	list := p.parseStmtList()
	p.closeLabelScope()
	p.closeScope()
	rbrace := p.expect2(token.RBRACE)

	return &ast.BlockStmt{Lbrace: lbrace, List: list, Rbrace: rbrace}
}

func (p *parser) parseBlockStmt() *ast.BlockStmt {
	if p.trace {
		defer un(trace(p, "BlockStmt"))
	}

	lbrace := p.expect(token.LBRACE)
	p.openScope()
	list := p.parseStmtList()
	p.closeScope()
	rbrace := p.expect2(token.RBRACE)

	return &ast.BlockStmt{Lbrace: lbrace, List: list, Rbrace: rbrace}
}

// ----------------------------------------------------------------------------
// Expressions

func (p *parser) parseFuncTypeOrLit() ast.Expr {
	if p.trace {
		defer un(trace(p, "FuncTypeOrLit"))
	}

	typ, scope := p.parseFuncType()
	if p.tok != token.LBRACE {
		// function type only
		return typ
	}

	p.exprLev++
	body := p.parseBody(scope)
	p.exprLev--

	return &ast.FuncLit{Type: typ, Body: body}
}

// parseOperand may return an expression or a raw type (incl. array
// types of the form [...]T. Callers must verify the result.
// If lhs is set and the result is an identifier, it is not resolved.
//
func (p *parser) parseOperand(lhs, allowTuple, allowCmd bool) (x ast.Expr, isTuple bool) {
	if p.trace {
		defer un(trace(p, "Operand"))
	}

	switch p.tok {
	case token.IDENT:
		x = p.parseIdent()
		if !lhs {
			p.resolve(x)
		}
		return

	case token.STRING, token.CSTRING, token.INT, token.FLOAT, token.IMAG, token.CHAR, token.RAT:
		x = &ast.BasicLit{ValuePos: p.pos, Kind: p.tok, Value: p.lit}
		if debugParseOutput {
			log.Printf("ast.BasicLit{Kind: %v, Value: %v}\n", p.tok, p.lit)
		}
		p.next()
		return

	case token.LPAREN:
		lparen := p.pos
		p.next()
		if allowTuple && p.tok == token.RPAREN { // () => expr
			p.next()
			return &tupleExpr{opening: lparen, closing: p.pos}, true
		}
		p.exprLev++
		x = p.parseRHSOrType() // types may be parenthesized: (some type)
		if allowTuple && (p.tok == token.COMMA || p.tok == token.ELLIPSIS) {
			// (x, y, ...) => expr
			items := make([]ast.Expr, 1, 2)
			items[0] = x
			for p.tok == token.COMMA {
				p.next()
				items = append(items, p.parseRHSOrType())
			}
			t := &tupleExpr{opening: lparen, items: items, closing: p.pos}
			if p.tok == token.ELLIPSIS {
				t.ellipsis = p.pos
				p.next()
			}
			p.exprLev--
			p.expect(token.RPAREN)
			return t, true
		}
		p.exprLev--
		rparen := p.expect(token.RPAREN)
		if debugParseOutput {
			log.Printf("ast.ParenExpr{X: %v}\n", x)
		}
		return &ast.ParenExpr{Lparen: lparen, X: x, Rparen: rparen}, false

	case token.FUNC:
		return p.parseFuncTypeOrLit(), false

	case token.LBRACE:
		if !lhs { // rhs: mapLit - {k1: v1, k2: v2, ...}
			return p.parseLiteralValueOrMapComprehension(), false
		}

	case token.MAP:
		oldpos, oldlit := p.pos, p.lit // Go+: save token to allow map() as a function
		p.next()
		pos, tok := p.pos, p.tok
		p.unget(oldpos, token.MAP, oldlit)
		if tok == token.LBRACK && (!allowCmd || oldpos+3 == pos) {
			break
		}
		fallthrough
	case token.GOTO, token.BREAK, token.CONTINUE, token.FALLTHROUGH:
		// token.RANGE, token.IMPORT, token.TYPE, token.SELECT, token.INTERFACE:
		// Go+: allow goto() as a function
		p.tok = token.IDENT
		x = p.parseIdent()
		if !lhs {
			p.resolve(x)
		}
		return
	}

	typ, result := p.tryIdentOrType(stateArrayTypeOrSliceLit, nil)
	if (result & resultExprFlags) != 0 { // is an expr, not a type
		return typ, false
	}
	if typ != nil {
		// could be type for composite literal or conversion
		_, isIdent := typ.(*ast.Ident)
		assert(!isIdent, "type cannot be identifier")
		return typ, false
	}

	// we have an error
	pos := p.pos
	p.errorExpected(pos, "operand", 2)
	p.advance(stmtStart)
	return &ast.BadExpr{From: pos, To: p.pos}, false
}

func (p *parser) parseSelector(x ast.Expr) ast.Expr {
	if p.trace {
		defer un(trace(p, "Selector"))
	}

	sel := p.parseIdent()

	return &ast.SelectorExpr{X: x, Sel: sel}
}

func (p *parser) parseTypeAssertion(x ast.Expr) ast.Expr {
	if p.trace {
		defer un(trace(p, "TypeAssertion"))
	}

	lparen := p.expect(token.LPAREN)
	var typ ast.Expr
	if p.tok == token.TYPE {
		// type switch: typ == nil
		p.next()
	} else {
		typ = p.parseType()
	}
	rparen := p.expect(token.RPAREN)

	return &ast.TypeAssertExpr{X: x, Type: typ, Lparen: lparen, Rparen: rparen}
}

func (p *parser) parseIndexOrSlice(x ast.Expr) ast.Expr {
	if p.trace {
		defer un(trace(p, "IndexOrSlice"))
	}
	lbrack := p.expect(token.LBRACK)
	p.exprLev++

	var idx ast.Expr
	if p.tok != token.COLON {
		idx = p.parseRHS()
	}
	return p.parseIndexOrSliceContinue(x, lbrack, idx)
}

func (p *parser) parseIndexOrSliceContinue(x ast.Expr, lbrack token.Pos, idx ast.Expr) ast.Expr {
	const N = 3 // change the 3 to 2 to disable 3-index slices
	var index [N]ast.Expr
	var colons [N - 1]token.Pos
	if idx != nil {
		index[0] = idx
	}
	ncolons := 0
	for p.tok == token.COLON && ncolons < len(colons) {
		colons[ncolons] = p.pos
		ncolons++
		p.next()
		if p.tok != token.COLON && p.tok != token.RBRACK && p.tok != token.EOF {
			index[ncolons] = p.parseRHS()
		}
	}
	p.exprLev--
	rbrack := p.expect(token.RBRACK)

	if ncolons > 0 {
		// slice expression
		slice3 := false
		if ncolons == 2 {
			slice3 = true
			// Check presence of 2nd and 3rd index here rather than during type-checking
			// to prevent erroneous programs from passing through gofmt (was issue 7305).
			if index[1] == nil {
				p.error(colons[0], "2nd index required in 3-index slice")
				index[1] = &ast.BadExpr{From: colons[0] + 1, To: colons[1]}
			}
			if index[2] == nil {
				p.error(colons[1], "3rd index required in 3-index slice")
				index[2] = &ast.BadExpr{From: colons[1] + 1, To: rbrack}
			}
		}
		if debugParseOutput {
			log.Printf("ast.SliceExpr{X: %v, Low: %v, High: %v, Max: %v, Slice3: %v}\n", x, index[0], index[1], index[2], slice3)
		}
		return &ast.SliceExpr{X: x, Lbrack: lbrack, Low: index[0], High: index[1], Max: index[2], Slice3: slice3, Rbrack: rbrack}
	}

	if debugParseOutput {
		log.Printf("ast.IndexExpr{X: %v, Index: %v}\n", x, index[0])
	}
	return &ast.IndexExpr{X: x, Lbrack: lbrack, Index: index[0], Rbrack: rbrack}
}

func (p *parser) parseCallOrConversion(fun ast.Expr, isCmd bool) *ast.CallExpr {
	if p.trace {
		defer un(trace(p, "CallOrConversion"))
	}
	var lparen, rparen token.Pos
	var endTok token.Token
	if isCmd {
		endTok = token.SEMICOLON
	} else {
		lparen, endTok = p.expect(token.LPAREN), token.RPAREN
	}
	p.exprLev++
	var list []ast.Expr
	var ellipsis token.Pos
	for p.tok != endTok && p.tok != token.EOF && !ellipsis.IsValid() {
		expr, isTuple := p.parseRHSOrTypeEx(isCmd && len(list) == 0)
		if isTuple {
			t := expr.(*tupleExpr)
			if p.tok != token.SEMICOLON && p.tok != token.RBRACE && p.tok != token.EOF {
				p.error(t.opening, msgTupleNotSupported)
				p.advance(stmtStart)
			}
			list, lparen, ellipsis, rparen = t.items, t.opening, t.ellipsis, t.closing
			isCmd = true
			break
		}
		list = append(list, expr) // builtins may expect a type: make(some type, ...)
		if p.tok == token.ELLIPSIS {
			ellipsis = p.pos
			p.next()
			if p.tok != token.RPAREN {
				break
			}
		}
		if isCmd && p.tok == token.RBRACE {
			break
		}
		if !p.atComma("argument list", endTok) {
			break
		}
		p.next()
	}
	p.exprLev--
	var noParenEnd token.Pos
	if isCmd {
		noParenEnd = p.pos
	} else if rparen == token.NoPos {
		rparen = p.expectClosing(token.RPAREN, "argument list")
	}
	if debugParseOutput {
		log.Printf("ast.CallExpr{Fun: %v, Ellipsis: %v, isCmd: %v}\n", fun, ellipsis != 0, isCmd)
	}
	return &ast.CallExpr{
		Fun: fun, Lparen: lparen, Args: list, Ellipsis: ellipsis, Rparen: rparen, NoParenEnd: noParenEnd}
}

func (p *parser) parseValue(keyOk bool) ast.Expr {
	if p.trace {
		defer un(trace(p, "Element"))
	}

	if p.tok == token.LBRACE {
		return p.parseLiteralValueOrMapComprehension()
	}

	// Because the parser doesn't know the composite literal type, it cannot
	// know if a key that's an identifier is a struct field name or a name
	// denoting a value. The former is not resolved by the parser or the
	// resolver.
	//
	// Instead, _try_ to resolve such a key if possible. If it resolves,
	// it a) has correctly resolved, or b) incorrectly resolved because
	// the key is a struct field with a name matching another identifier.
	// In the former case we are done, and in the latter case we don't
	// care because the type checker will do a separate field lookup.
	//
	// If the key does not resolve, it a) must be defined at the top
	// level in another file of the same package, the universe scope, or be
	// undeclared; or b) it is a struct field. In the former case, the type
	// checker can do a top-level lookup, and in the latter case it will do
	// a separate field lookup.
	x := p.checkExpr(p.parseExpr(keyOk, false, false))
	if keyOk {
		if p.tok == token.COLON {
			// Try to resolve the key but don't collect it
			// as unresolved identifier if it fails so that
			// we don't get (possibly false) errors about
			// undeclared names.
			p.tryResolve(x, false)
		} else {
			// not a key
			p.resolve(x)
		}
	}

	return x
}

func (p *parser) parseElement() ast.Expr {
	if p.trace {
		defer un(trace(p, "Element"))
	}

	x := p.parseValue(true)
	if p.tok == token.COLON {
		colon := p.pos
		p.next()
		x = &ast.KeyValueExpr{Key: x, Colon: colon, Value: p.parseValue(false)}
	}

	return x
}

// {k1: v1, k2: v2, ...}
// {for k, v <- listOrMap, cond}
// {expr for k, v <- listOrMap, cond}
// {kexpr: vexpr for k, v <- listOrMap, cond}
func (p *parser) parseLiteralValueOrMapComprehension() ast.Expr {
	if p.trace {
		defer un(trace(p, "LiteralValue"))
	}

	lbrace := p.expect(token.LBRACE)
	var elts []ast.Expr
	var mce *ast.ComprehensionExpr
	p.exprLev++
	if p.tok != token.RBRACE {
		elts, mce = p.parseElementListOrComprehension()
	}
	p.exprLev--
	rbrace := p.expectClosing(token.RBRACE, "composite literal")
	if mce != nil {
		mce.Lpos, mce.Rpos, mce.Tok = lbrace, rbrace, token.LBRACE
		return mce
	}
	return &ast.CompositeLit{Lbrace: lbrace, Elts: elts, Rbrace: rbrace}
}

func (p *parser) parseElementListOrComprehension() (list []ast.Expr, mce *ast.ComprehensionExpr) {
	if p.trace {
		defer un(trace(p, "ElementList"))
	}

	if p.tok == token.FOR {
		phrases := p.parseForPhrases()
		return nil, &ast.ComprehensionExpr{Fors: phrases}
	}
	for p.tok != token.RBRACE && p.tok != token.EOF {
		list = append(list, p.parseElement())
		if p.tok == token.FOR { // for k, v <- container
			if len(list) != 1 {
				log.Panicln("TODO: invalid comprehension: too may elements.")
			}
			phrases := p.parseForPhrases()
			return nil, &ast.ComprehensionExpr{Elt: list[0], Fors: phrases}
		}
		if !p.atComma("composite literal", token.RBRACE) {
			break
		}
		p.next()
	}
	return
}

func (p *parser) parseElementList() (list []ast.Expr) {
	if p.trace {
		defer un(trace(p, "ElementList"))
	}

	for p.tok != token.RBRACE && p.tok != token.EOF {
		list = append(list, p.parseElement())
		if !p.atComma("composite literal", token.RBRACE) {
			break
		}
		p.next()
	}

	return
}

func (p *parser) parseLiteralValue(typ ast.Expr) ast.Expr {
	if p.trace {
		defer un(trace(p, "LiteralValue"))
	}

	lbrace := p.expect(token.LBRACE)
	var elts []ast.Expr
	p.exprLev++
	if p.tok != token.RBRACE {
		elts = p.parseElementList()
	}
	p.exprLev--
	rbrace := p.expectClosing(token.RBRACE, "composite literal")
	return &ast.CompositeLit{Type: typ, Lbrace: lbrace, Elts: elts, Rbrace: rbrace}
}

// checkExpr checks that x is an expression (and not a type).
func (p *parser) checkExpr(x ast.Expr) ast.Expr {
	switch v := unparen(x).(type) {
	case *ast.BadExpr:
	case *ast.Ident:
	case *ast.BasicLit:
	case *ast.FuncLit:
	case *ast.CompositeLit:
	case *ast.SliceLit:
	case *ast.ComprehensionExpr:
	case *ast.SelectorExpr:
	case *ast.IndexExpr:
	case *ast.SliceExpr:
	case *ast.TypeAssertExpr:
		// If t.Type == nil we have a type assertion of the form
		// y.(type), which is only allowed in type switch expressions.
		// It's hard to exclude those but for the case where we are in
		// a type switch. Instead be lenient and test this in the type
		// checker.
	case *ast.CallExpr:
	case *ast.StarExpr:
	case *ast.UnaryExpr:
	case *ast.BinaryExpr:
	case *ast.RangeExpr:
	case *ast.ErrWrapExpr:
	case *ast.LambdaExpr:
	case *ast.LambdaExpr2:
	case *tupleExpr:
		p.error(v.opening, msgTupleNotSupported)
		x = &ast.BadExpr{From: v.opening, To: v.closing}
	default:
		// all other nodes are not proper expressions
		p.errorExpected(x.Pos(), "expression", 3)
		x = &ast.BadExpr{From: x.Pos(), To: p.safePos(x.End())}
	}
	return x
}

// isTypeName reports whether x is a (qualified) TypeName.
func isTypeName(x ast.Expr) bool {
	switch t := x.(type) {
	case *ast.BadExpr:
	case *ast.Ident:
	case *ast.SelectorExpr:
		_, isIdent := t.X.(*ast.Ident)
		return isIdent
	default:
		return false // all other nodes are not type names
	}
	return true
}

// isLiteralType reports whether x is a legal composite literal type.
func isLiteralType(x ast.Expr) bool {
	switch t := x.(type) {
	case *ast.BadExpr:
	case *ast.Ident:
	case *ast.SelectorExpr:
		_, isIdent := t.X.(*ast.Ident)
		return isIdent
	case *ast.ArrayType:
	case *ast.StructType:
	case *ast.MapType:
	default:
		return false // all other nodes are not legal composite literal types
	}
	return true
}

// If x is of the form *T, deref returns T, otherwise it returns x.
func deref(x ast.Expr) ast.Expr {
	if p, isPtr := x.(*ast.StarExpr); isPtr {
		x = p.X
	}
	return x
}

// If x is of the form (T), unparen returns unparen(T), otherwise it returns x.
func unparen(x ast.Expr) ast.Expr {
	if p, isParen := x.(*ast.ParenExpr); isParen {
		x = unparen(p.X)
	}
	return x
}

// checkExprOrType checks that x is an expression or a type
// (and not a raw type such as [...]T).
func (p *parser) checkExprOrType(x ast.Expr) ast.Expr {
	switch t := unparen(x).(type) {
	case *ast.ArrayType:
		if len, isEllipsis := t.Len.(*ast.Ellipsis); isEllipsis {
			p.error(len.Pos(), "expected array length, found '...'")
			x = &ast.BadExpr{From: x.Pos(), To: p.safePos(x.End())}
		}
	}
	// all other nodes are expressions or types
	return x
}

// If lhs is set and the result is an identifier, it is not resolved.
func (p *parser) parsePrimaryExpr(lhs, allowTuple, allowCmd bool) (x ast.Expr, isTuple bool) {
	if p.trace {
		defer un(trace(p, "PrimaryExpr"))
	}

	if x, isTuple = p.parseOperand(lhs, allowTuple, allowCmd); isTuple {
		return
	}
L:
	for {
		switch p.tok {
		case token.PERIOD:
			p.next()
			if lhs {
				p.resolve(x)
			}
			switch p.tok {
			case token.IDENT:
				x = p.parseSelector(p.checkExprOrType(x))
			case token.LPAREN:
				x = p.parseTypeAssertion(p.checkExpr(x))
			case token.GOTO, token.BREAK, token.CONTINUE, token.FALLTHROUGH:
				// Go+: allow goto() as a function
				p.tok = token.IDENT
				x = p.parseSelector(p.checkExprOrType(x))
			default:
				pos := p.pos
				p.errorExpected(pos, "selector or type assertion", 2)
				p.next() // make progress
				sel := &ast.Ident{NamePos: pos, Name: "_"}
				x = &ast.SelectorExpr{X: x, Sel: sel}
			}
		case token.LBRACK: // [
			if lhs {
				p.resolve(x)
			}
			if allowCmd && p.isCmd(x) { // println [...]
				x = p.parseCallOrConversion(p.checkExprOrType(x), true)
			} else {
				x = p.parseIndexOrSlice(p.checkExpr(x))
			}
		case token.LPAREN: // (
			if lhs {
				p.resolve(x)
			}
			isCmd := allowCmd && p.isCmd(x) // println (...)
			x = p.parseCallOrConversion(p.checkExprOrType(x), isCmd)
		case token.LBRACE: // {
			if allowCmd && p.isCmd(x) { // println {...}
				x = p.parseCallOrConversion(p.checkExprOrType(x), true)
			} else if isLiteralType(x) && (p.exprLev >= 0 || !isTypeName(x)) {
				if lhs {
					p.resolve(x)
				}
				x = p.parseLiteralValue(x)
			} else {
				break L
			}
		case token.NOT, token.QUESTION:
			x = &ast.ErrWrapExpr{X: x, Tok: p.tok, TokPos: p.pos}
			p.next()
		default:
			if allowCmd && p.isCmd(x) && p.checkCmd(x) {
				if lhs {
					p.resolve(x)
				}
				x = p.parseCallOrConversion(p.checkExprOrType(x), true)
			} else {
				break L
			}
		}
		lhs = false // no need to try to resolve again
	}
	return
}

func (p *parser) isCmd(x ast.Expr) bool {
	switch x.(type) {
	case *ast.Ident, *ast.SelectorExpr, *ast.ErrWrapExpr:
		return x.End() != p.pos
	}
	return false
}

func (p *parser) checkCmd(x ast.Expr) bool {
	switch p.tok {
	case token.IDENT, token.RARROW,
		token.STRING, token.CSTRING, token.INT, token.FLOAT, token.IMAG, token.CHAR, token.RAT,
		token.FUNC, token.GOTO, token.MAP, token.INTERFACE, token.CHAN, token.STRUCT:
		return true
	case token.SUB, token.AND, token.MUL, token.ARROW, token.XOR, token.ADD:
		oldtok, oldpos := p.tok, p.pos
		p.next()
		newpos := int(p.pos)
		p.unget(oldpos, oldtok, "")
		return int(oldpos)+len(oldtok.String()) == newpos // x -y
	}
	return false
}

// parseErrWrapExpr: expr! expr? expr?:defval
func (p *parser) parseErrWrapExpr(lhs, allowTuple, allowCmd bool) (x ast.Expr, isTuple bool) {
	if x, isTuple = p.parsePrimaryExpr(lhs, allowTuple, allowCmd); isTuple {
		return
	}
	if expr, ok := x.(*ast.ErrWrapExpr); ok {
		if p.tok == token.COLON {
			p.next()
			expr.Default, _ = p.parseUnaryExpr(false, false, false)
		}
	}
	return
}

// If lhs is set and the result is an identifier, it is not resolved.
func (p *parser) parseUnaryExpr(lhs, allowTuple, allowCmd bool) (ast.Expr, bool) {
	if p.trace {
		defer un(trace(p, "UnaryExpr"))
	}

	switch p.tok {
	case token.ADD, token.SUB, token.NOT, token.XOR, token.AND:
		pos, op := p.pos, p.tok
		p.next()
		x, _ := p.parseUnaryExpr(false, false, false)
		return &ast.UnaryExpr{OpPos: pos, Op: op, X: p.checkExpr(x)}, false

	case token.ARROW:
		// channel type or receive expression
		arrow := p.pos
		p.next()

		// If the next token is token.CHAN we still don't know if it
		// is a channel type or a receive operation - we only know
		// once we have found the end of the unary expression. There
		// are two cases:
		//
		//   <- type  => (<-type) must be channel type
		//   <- expr  => <-(expr) is a receive from an expression
		//
		// In the first case, the arrow must be re-associated with
		// the channel type parsed already:
		//
		//   <- (chan type)    =>  (<-chan type)
		//   <- (chan<- type)  =>  (<-chan (<-type))

		x, _ := p.parseUnaryExpr(false, false, false)

		// determine which case we have
		if typ, ok := x.(*ast.ChanType); ok {
			// (<-type)

			// re-associate position info and <-
			dir := ast.SEND
			for ok && dir == ast.SEND {
				if typ.Dir == ast.RECV {
					// error: (<-type) is (<-(<-chan T))
					p.errorExpected(typ.Arrow, "'chan'", 2)
				}
				arrow, typ.Begin, typ.Arrow = typ.Arrow, arrow, arrow
				dir, typ.Dir = typ.Dir, ast.RECV
				typ, ok = typ.Value.(*ast.ChanType)
			}
			if dir == ast.SEND {
				p.errorExpected(arrow, "channel type", 2)
			}

			return x, false
		}

		// <-(expr)
		return &ast.UnaryExpr{OpPos: arrow, Op: token.ARROW, X: p.checkExpr(x)}, false

	case token.MUL:
		// pointer type or unary "*" expression
		pos := p.pos
		p.next()
		x, _ := p.parseUnaryExpr(false, false, false)
		return &ast.StarExpr{Star: pos, X: p.checkExprOrType(x)}, false
	}

	return p.parseErrWrapExpr(lhs, allowTuple, allowCmd)
}

func (p *parser) tokPrec() (token.Token, int) {
	tok := p.tok
	if p.inRHS && tok == token.ASSIGN {
		tok = token.EQL
	}
	return tok, tok.Precedence()
}

// If lhs is set and the result is an identifier, it is not resolved.
func (p *parser) parseBinaryExpr(lhs bool, prec1 int, allowTuple, allowCmd bool) (x ast.Expr, isTuple bool) {
	if p.trace {
		defer un(trace(p, "BinaryExpr"))
	}

	if x, isTuple = p.parseUnaryExpr(lhs, allowTuple, allowCmd); isTuple {
		return
	}
	for {
		op, oprec := p.tokPrec()
		if oprec < prec1 {
			return
		}
		pos := p.expect(op)
		if lhs {
			p.resolve(x)
			lhs = false
		}
		y, _ := p.parseBinaryExpr(false, oprec+1, false, false)
		x = &ast.BinaryExpr{X: p.checkExpr(x), OpPos: pos, Op: op, Y: p.checkExpr(y)}
	}
}

func (p *parser) parseRangeExpr(allowTuple, allowCmd bool) (x ast.Expr, isTuple bool) {
	if p.tok != token.COLON {
		x, isTuple = p.parseBinaryExpr(false, token.LowestPrec+1, allowTuple, allowCmd)
		if isTuple || p.tok != token.COLON { // not RangeExpr
			return
		}
	}
	to := p.pos
	p.next()
	high, _ := p.parseBinaryExpr(false, token.LowestPrec+1, false, false)
	var colon2 token.Pos
	var expr3 ast.Expr
	if p.tok == token.COLON {
		colon2 = p.pos
		p.next()
		expr3, _ = p.parseBinaryExpr(false, token.LowestPrec+1, false, false)
	}
	if debugParseOutput {
		log.Printf("ast.RangeExpr{First: %v, Last: %v, Expr3: %v}\n", x, high, expr3)
	}
	return &ast.RangeExpr{First: x, To: to, Last: high, Colon2: colon2, Expr3: expr3}, false
}

type tupleExpr struct {
	ast.Expr
	opening  token.Pos
	items    []ast.Expr
	ellipsis token.Pos
	closing  token.Pos
}

func (p *parser) parseLambdaExpr(allowTuple, allowCmd, allowRangeExpr bool) (x ast.Expr, isTuple bool) {
	var first = p.pos
	if p.tok != token.RARROW {
		if allowRangeExpr {
			x, isTuple = p.parseRangeExpr(true, allowCmd)
		} else {
			x, isTuple = p.parseBinaryExpr(false, token.LowestPrec+1, true, allowCmd)
		}
	}
	if p.tok == token.RARROW { // =>
		var rarrow = p.pos
		var rhs []ast.Expr
		var body *ast.BlockStmt
		var lhsHasParen, rhsHasParen bool
		p.next()
		switch p.tok {
		case token.LPAREN: // (
			rhsHasParen = true
			p.next()
			for {
				item := p.parseExpr(false, false, false)
				rhs = append(rhs, item)
				if p.tok != token.COMMA {
					break
				}
				p.next()
			}
			p.expect(token.RPAREN)
		case token.LBRACE: // {
			body = p.parseBlockStmt()
		default:
			rhs = []ast.Expr{p.parseExpr(false, false, false)}
		}
		var lhs []*ast.Ident
		if x != nil {
			e := x
		retry:
			switch v := e.(type) {
			case *tupleExpr:
				items := make([]*ast.Ident, len(v.items))
				for i, item := range v.items {
					items[i] = p.toIdent(item)
				}
				lhs, lhsHasParen = items, true
			case *ast.ParenExpr:
				e, lhsHasParen = v.X, true
				goto retry
			default:
				lhs = []*ast.Ident{p.toIdent(v)}
			}
		}
		if debugParseOutput {
			log.Printf("ast.LambdaExpr{Lhs: %v}\n", lhs)
		}
		if body != nil {
			return &ast.LambdaExpr2{
				First:       first,
				Lhs:         lhs,
				Rarrow:      rarrow,
				Body:        body,
				LhsHasParen: lhsHasParen,
			}, false
		}
		return &ast.LambdaExpr{
			First:       first,
			Last:        p.pos,
			Lhs:         lhs,
			Rarrow:      rarrow,
			Rhs:         rhs,
			LhsHasParen: lhsHasParen,
			RhsHasParen: rhsHasParen,
		}, false
	} else if isTuple && !allowTuple {
		p.error(x.(*tupleExpr).opening, msgTupleNotSupported)
		p.advance(stmtStart)
	}
	return
}

// If lhs is set and the result is an identifier, it is not resolved.
// The result may be a type or even a raw type ([...]int). Callers must
// check the result (using checkExpr or checkExprOrType), depending on
// context.
func (p *parser) parseExprEx(lhs, allowTuple, allowCmd, allowRangeExpr bool) (ast.Expr, bool) {
	if p.trace {
		defer un(trace(p, "Expression"))
	}
	if lhs {
		return p.parseBinaryExpr(true, token.LowestPrec+1, false, allowCmd)
	}
	return p.parseLambdaExpr(allowTuple, allowCmd, allowRangeExpr)
}

func (p *parser) parseExpr(lhs, allowCmd, allowRangeExpr bool) ast.Expr {
	x, _ := p.parseExprEx(lhs, false, allowCmd, allowRangeExpr)
	return x
}

func (p *parser) parseRHS() ast.Expr {
	return p.parseRHSEx(false)
}

func (p *parser) parseRHSEx(allowRangeExpr bool) ast.Expr {
	old := p.inRHS
	p.inRHS = true
	x := p.checkExpr(p.parseExpr(false, false, allowRangeExpr))
	p.inRHS = old
	return x
}

func (p *parser) parseRHSOrTypeEx(allowTuple bool) (x ast.Expr, isTuple bool) {
	old := p.inRHS
	p.inRHS = true
	x, isTuple = p.parseExprEx(false, allowTuple, false, false)
	if !isTuple {
		x = p.checkExprOrType(x)
	}
	p.inRHS = old
	return
}

func (p *parser) parseRHSOrType() ast.Expr {
	x, _ := p.parseRHSOrTypeEx(false)
	return x
}

// ----------------------------------------------------------------------------
// Statements

// Parsing modes for parseSimpleStmt.
const (
	basic = iota
	labelOk
	rangeOk
)

// parseSimpleStmt returns true as 2nd result if it parsed the assignment
// of a range clause (with mode == rangeOk). The returned statement is an
// assignment with a right-hand side that is a single unary expression of
// the form "range x". No guarantees are given for the left-hand side.
func (p *parser) parseSimpleStmt(mode int, allowCmd bool) (ast.Stmt, bool) {
	if p.trace {
		defer un(trace(p, "SimpleStmt"))
	}

	x := p.parseLHSList(allowCmd)

	switch p.tok {
	case
		token.DEFINE, token.ASSIGN, token.ADD_ASSIGN,
		token.SUB_ASSIGN, token.MUL_ASSIGN, token.QUO_ASSIGN,
		token.REM_ASSIGN, token.AND_ASSIGN, token.OR_ASSIGN,
		token.XOR_ASSIGN, token.SHL_ASSIGN, token.SHR_ASSIGN, token.AND_NOT_ASSIGN:
		// assignment statement, possibly part of a range clause
		pos, tok := p.pos, p.tok
		p.next()
		var y []ast.Expr
		isRange := false
		if mode == rangeOk && p.tok == token.RANGE && (tok == token.DEFINE || tok == token.ASSIGN) {
			pos := p.pos
			p.next()
			y = []ast.Expr{&ast.UnaryExpr{OpPos: pos, Op: token.RANGE, X: p.parseRHSEx(true)}}
			isRange = true
		} else {
			y = p.parseRHSList()
		}
		as := &ast.AssignStmt{Lhs: x, TokPos: pos, Tok: tok, Rhs: y}
		if tok == token.DEFINE {
			p.shortVarDecl(as, x)
		}
		return as, isRange
	case token.ARROW:
		if mode == rangeOk {
			return p.parseForPhraseStmtPart(x), true
		}
	}

	if len(x) > 1 {
		p.errorExpected(x[0].Pos(), "1 expression", 2)
		// continue with first expression
	}

	switch p.tok {
	case token.COLON:
		// labeled statement
		colon := p.pos
		p.next()
		if label, isIdent := x[0].(*ast.Ident); mode == labelOk && isIdent {
			// Go spec: The scope of a label is the body of the function
			// in which it is declared and excludes the body of any nested
			// function.
			stmt := &ast.LabeledStmt{Label: label, Colon: colon, Stmt: p.parseStmt(allowCmd)}
			p.declare(stmt, nil, p.labelScope, ast.Lbl, label)
			return stmt, false
		}
		// The label declaration typically starts at x[0].Pos(), but the label
		// declaration may be erroneous due to a token after that position (and
		// before the ':'). If SpuriousErrors is not set, the (only) error
		// reported for the line is the illegal label error instead of the token
		// before the ':' that caused the problem. Thus, use the (latest) colon
		// position for error reporting.
		p.error(colon, "illegal label declaration")
		return &ast.BadStmt{From: x[0].Pos(), To: colon + 1}, false

	case token.ARROW:
		// send statement
		arrow := p.pos
		p.next()
		y := p.parseRHS()
		return &ast.SendStmt{Chan: x[0], Arrow: arrow, Value: y}, false

	case token.INC, token.DEC:
		// increment or decrement
		s := &ast.IncDecStmt{X: x[0], TokPos: p.pos, Tok: p.tok}
		p.next()
		return s, false
	}

	// expression
	return &ast.ExprStmt{X: x[0]}, false
}

func (p *parser) parseCallExpr(callType string) *ast.CallExpr {
	x := p.parseRHSOrType() // could be a conversion: (some type)(x)
	if call, isCall := x.(*ast.CallExpr); isCall {
		return call
	}
	if _, isBad := x.(*ast.BadExpr); !isBad {
		// only report error if it's a new one
		p.error(p.safePos(x.End()), fmt.Sprintf("function must be invoked in %s statement", callType))
	}
	return nil
}

func (p *parser) parseGoStmt() ast.Stmt {
	if p.trace {
		defer un(trace(p, "GoStmt"))
	}

	pos := p.expect(token.GO)
	call := p.parseCallExpr("go")
	p.expectSemi()
	if call == nil {
		return &ast.BadStmt{From: pos, To: pos + 2} // len("go")
	}

	return &ast.GoStmt{Go: pos, Call: call}
}

func (p *parser) parseDeferStmt() ast.Stmt {
	if p.trace {
		defer un(trace(p, "DeferStmt"))
	}

	pos := p.expect(token.DEFER)
	call := p.parseCallExpr("defer")
	p.expectSemi()
	if call == nil {
		return &ast.BadStmt{From: pos, To: pos + 5} // len("defer")
	}

	return &ast.DeferStmt{Defer: pos, Call: call}
}

func (p *parser) parseReturnStmt() *ast.ReturnStmt {
	if p.trace {
		defer un(trace(p, "ReturnStmt"))
	}

	pos := p.pos
	p.expect(token.RETURN)
	var x []ast.Expr
	if p.tok != token.SEMICOLON && p.tok != token.RBRACE {
		x = p.parseRHSList()
	}
	p.expectSemi()

	return &ast.ReturnStmt{Return: pos, Results: x}
}

func (p *parser) parseBranchStmt(tok token.Token) ast.Stmt {
	if p.trace {
		defer un(trace(p, "BranchStmt"))
	}

	oldpos, oldlit := p.pos, p.lit // Go+: save token to allow goto() as a function
	pos := p.expect(tok)
	if p.tok == token.LPAREN { // Go+: allow goto() as a function
		p.unget(oldpos, token.IDENT, oldlit)
		s, _ := p.parseSimpleStmt(basic, false)
		p.expectSemi()
		return s
	}

	var label *ast.Ident
	if tok != token.FALLTHROUGH && p.tok == token.IDENT {
		label = p.parseIdent()
		// add to list of unresolved targets
		n := len(p.targetStack) - 1
		p.targetStack[n] = append(p.targetStack[n], label)
	}
	p.expectSemi()

	return &ast.BranchStmt{TokPos: pos, Tok: tok, Label: label}
}

func (p *parser) makeExpr(s ast.Stmt, want string) ast.Expr {
	if s == nil {
		return nil
	}
	if es, isExpr := s.(*ast.ExprStmt); isExpr {
		return p.checkExpr(es.X)
	}
	found := "simple statement"
	if _, isAss := s.(*ast.AssignStmt); isAss {
		found = "assignment"
	}
	p.error(s.Pos(), fmt.Sprintf("expected %s, found %s (missing parentheses around composite literal?)", want, found))
	return &ast.BadExpr{From: s.Pos(), To: p.safePos(s.End())}
}

// parseIfHeader is an adjusted version of parser.header
// in cmd/compile/internal/syntax/parser.go, which has
// been tuned for better error handling.
func (p *parser) parseIfHeader() (init ast.Stmt, cond ast.Expr) {
	if p.tok == token.LBRACE {
		p.error(p.pos, "missing condition in if statement")
		cond = &ast.BadExpr{From: p.pos, To: p.pos}
		return
	}
	// p.tok != token.LBRACE

	outer := p.exprLev
	p.exprLev = -1

	if p.tok != token.SEMICOLON {
		// accept potential variable declaration but complain
		if p.tok == token.VAR {
			p.next()
			p.error(p.pos, "var declaration not allowed in 'IF' initializer")
		}
		init, _ = p.parseSimpleStmt(basic, false)
	}

	var condStmt ast.Stmt
	var semi struct {
		pos token.Pos
		lit string // ";" or "\n"; valid if pos.IsValid()
	}
	if p.tok != token.LBRACE {
		if p.tok == token.SEMICOLON {
			semi.pos = p.pos
			semi.lit = p.lit
			p.next()
		} else {
			p.expect(token.SEMICOLON)
		}
		if p.tok != token.LBRACE {
			condStmt, _ = p.parseSimpleStmt(basic, false)
		}
	} else {
		condStmt = init
		init = nil
	}

	if condStmt != nil {
		cond = p.makeExpr(condStmt, "boolean expression")
	} else if semi.pos.IsValid() {
		if semi.lit == "\n" {
			p.error(semi.pos, "unexpected newline, expecting { after if clause")
		} else {
			p.error(semi.pos, "missing condition in if statement")
		}
	}

	// make sure we have a valid AST
	if cond == nil {
		cond = &ast.BadExpr{From: p.pos, To: p.pos}
	}

	p.exprLev = outer
	return
}

func isForPhraseCondEnd(tok token.Token) bool {
	return tok == token.RBRACK || tok == token.RBRACE || tok == token.FOR
}

// parseForPhraseCond is an adjusted version of parseIfHeader
func (p *parser) parseForPhraseCond() (init ast.Stmt, cond ast.Expr) {
	if isForPhraseCondEnd(p.tok) {
		p.error(p.pos, "missing condition in for <- statement")
		cond = &ast.BadExpr{From: p.pos, To: p.pos}
		return
	}

	outer := p.exprLev
	p.exprLev = -1

	if p.tok != token.SEMICOLON {
		// accept potential variable declaration but complain
		if p.tok == token.VAR {
			p.next()
			p.error(p.pos, "var declaration not allowed in 'IF' initializer")
		}
		init, _ = p.parseSimpleStmt(basic, false)
	}

	var condStmt ast.Stmt
	var semi struct {
		pos token.Pos
		lit string // ";" or "\n"; valid if pos.IsValid()
	}
	if !isForPhraseCondEnd(p.tok) {
		if p.tok == token.SEMICOLON {
			semi.pos = p.pos
			semi.lit = p.lit
			p.next()
		} else {
			p.expect(token.SEMICOLON)
		}
		if !isForPhraseCondEnd(p.tok) {
			condStmt, _ = p.parseSimpleStmt(basic, false)
		}
	} else {
		condStmt = init
		init = nil
	}

	if condStmt != nil {
		cond = p.makeExpr(condStmt, "boolean expression")
	} else if semi.pos.IsValid() {
		p.error(semi.pos, "missing condition in for <- statement")
	}

	// make sure we have a valid AST
	if cond == nil {
		cond = &ast.BadExpr{From: p.pos, To: p.pos}
	}

	p.exprLev = outer
	return
}

func (p *parser) parseIfStmt() *ast.IfStmt {
	if p.trace {
		defer un(trace(p, "IfStmt"))
	}

	pos := p.expect(token.IF)
	p.openScope()
	defer p.closeScope()

	init, cond := p.parseIfHeader()
	body := p.parseBlockStmt()

	var elseStmt ast.Stmt
	if p.tok == token.ELSE {
		p.next()
		switch p.tok {
		case token.IF:
			elseStmt = p.parseIfStmt()
		case token.LBRACE:
			elseStmt = p.parseBlockStmt()
			p.expectSemi()
		default:
			p.errorExpected(p.pos, "if statement or block", 2)
			elseStmt = &ast.BadStmt{From: p.pos, To: p.pos}
		}
	} else {
		p.expectSemi()
	}

	return &ast.IfStmt{If: pos, Init: init, Cond: cond, Body: body, Else: elseStmt}
}

func (p *parser) parseTypeList() (list []ast.Expr) {
	if p.trace {
		defer un(trace(p, "TypeList"))
	}

	list = append(list, p.parseType())
	for p.tok == token.COMMA {
		p.next()
		list = append(list, p.parseType())
	}

	return
}

func (p *parser) parseCaseClause(typeSwitch bool) *ast.CaseClause {
	if p.trace {
		defer un(trace(p, "CaseClause"))
	}

	pos := p.pos
	var list []ast.Expr
	if p.tok == token.CASE {
		p.next()
		if typeSwitch {
			list = p.parseTypeList()
		} else {
			list = p.parseRHSList()
		}
	} else {
		p.expect(token.DEFAULT)
	}

	colon := p.expect(token.COLON)
	p.openScope()
	body := p.parseStmtList()
	p.closeScope()

	return &ast.CaseClause{Case: pos, List: list, Colon: colon, Body: body}
}

func isTypeSwitchAssert(x ast.Expr) bool {
	a, ok := x.(*ast.TypeAssertExpr)
	return ok && a.Type == nil
}

func (p *parser) isTypeSwitchGuard(s ast.Stmt) bool {
	switch t := s.(type) {
	case *ast.ExprStmt:
		// x.(type)
		return isTypeSwitchAssert(t.X)
	case *ast.AssignStmt:
		// v := x.(type)
		if len(t.Lhs) == 1 && len(t.Rhs) == 1 && isTypeSwitchAssert(t.Rhs[0]) {
			switch t.Tok {
			case token.ASSIGN:
				// permit v = x.(type) but complain
				p.error(t.TokPos, "expected ':=', found '='")
				fallthrough
			case token.DEFINE:
				return true
			}
		}
	}
	return false
}

func (p *parser) parseSwitchStmt() ast.Stmt {
	if p.trace {
		defer un(trace(p, "SwitchStmt"))
	}

	pos := p.expect(token.SWITCH)
	p.openScope()
	defer p.closeScope()

	var s1, s2 ast.Stmt
	if p.tok != token.LBRACE {
		prevLev := p.exprLev
		p.exprLev = -1
		if p.tok != token.SEMICOLON {
			s2, _ = p.parseSimpleStmt(basic, false)
		}
		if p.tok == token.SEMICOLON {
			p.next()
			s1 = s2
			s2 = nil
			if p.tok != token.LBRACE {
				// A TypeSwitchGuard may declare a variable in addition
				// to the variable declared in the initial SimpleStmt.
				// Introduce extra scope to avoid redeclaration errors:
				//
				//	switch t := 0; t := x.(T) { ... }
				//
				// (this code is not valid Go because the first t
				// cannot be accessed and thus is never used, the extra
				// scope is needed for the correct error message).
				//
				// If we don't have a type switch, s2 must be an expression.
				// Having the extra nested but empty scope won't affect it.
				p.openScope()
				defer p.closeScope()
				s2, _ = p.parseSimpleStmt(basic, false)
			}
		}
		p.exprLev = prevLev
	}

	typeSwitch := p.isTypeSwitchGuard(s2)
	lbrace := p.expect(token.LBRACE)
	var list []ast.Stmt
	for p.tok == token.CASE || p.tok == token.DEFAULT {
		list = append(list, p.parseCaseClause(typeSwitch))
	}
	rbrace := p.expect(token.RBRACE)
	p.expectSemi()
	body := &ast.BlockStmt{Lbrace: lbrace, List: list, Rbrace: rbrace}

	if typeSwitch {
		return &ast.TypeSwitchStmt{Switch: pos, Init: s1, Assign: s2, Body: body}
	}

	return &ast.SwitchStmt{Switch: pos, Init: s1, Tag: p.makeExpr(s2, "switch expression"), Body: body}
}

func (p *parser) parseCommClause() *ast.CommClause {
	if p.trace {
		defer un(trace(p, "CommClause"))
	}

	p.openScope()
	pos := p.pos
	var comm ast.Stmt
	if p.tok == token.CASE {
		p.next()
		lhs := p.parseLHSList(false)
		if p.tok == token.ARROW {
			// SendStmt
			if len(lhs) > 1 {
				p.errorExpected(lhs[0].Pos(), "1 expression", 2)
				// continue with first expression
			}
			arrow := p.pos
			p.next()
			rhs := p.parseRHS()
			comm = &ast.SendStmt{Chan: lhs[0], Arrow: arrow, Value: rhs}
		} else {
			// RecvStmt
			if tok := p.tok; tok == token.ASSIGN || tok == token.DEFINE {
				// RecvStmt with assignment
				if len(lhs) > 2 {
					p.errorExpected(lhs[0].Pos(), "1 or 2 expressions", 2)
					// continue with first two expressions
					lhs = lhs[0:2]
				}
				pos := p.pos
				p.next()
				rhs := p.parseRHS()
				as := &ast.AssignStmt{Lhs: lhs, TokPos: pos, Tok: tok, Rhs: []ast.Expr{rhs}}
				if tok == token.DEFINE {
					p.shortVarDecl(as, lhs)
				}
				comm = as
			} else {
				// lhs must be single receive operation
				if len(lhs) > 1 {
					p.errorExpected(lhs[0].Pos(), "1 expression", 2)
					// continue with first expression
				}
				comm = &ast.ExprStmt{X: lhs[0]}
			}
		}
	} else {
		p.expect(token.DEFAULT)
	}

	colon := p.expect(token.COLON)
	body := p.parseStmtList()
	p.closeScope()

	return &ast.CommClause{Case: pos, Comm: comm, Colon: colon, Body: body}
}

func (p *parser) parseSelectStmt() *ast.SelectStmt {
	if p.trace {
		defer un(trace(p, "SelectStmt"))
	}

	pos := p.expect(token.SELECT)
	lbrace := p.expect(token.LBRACE)
	var list []ast.Stmt
	for p.tok == token.CASE || p.tok == token.DEFAULT {
		list = append(list, p.parseCommClause())
	}
	rbrace := p.expect(token.RBRACE)
	p.expectSemi()
	body := &ast.BlockStmt{Lbrace: lbrace, List: list, Rbrace: rbrace}

	return &ast.SelectStmt{Select: pos, Body: body}
}

func (p *parser) parseForPhrases() (phrases []*ast.ForPhrase) {
	for {
		phrase := p.parseForPhrase()
		phrases = append(phrases, phrase)
		if p.tok != token.FOR {
			return
		}
	}
}

func (p *parser) parseForPhraseStmtPart(lhs []ast.Expr) *ast.ForPhraseStmt {
	tokPos := p.expect(token.ARROW) // <-
	x := p.parseExpr(false, false, true)
	var cond ast.Expr
	var ifPos token.Pos
	if p.tok == token.IF || p.tok == token.COMMA {
		ifPos = p.pos
		p.next()
		cond = p.parseExpr(false, false, false)
	}

	stmt := &ast.ForPhraseStmt{ForPhrase: &ast.ForPhrase{TokPos: tokPos, X: x, IfPos: ifPos, Cond: cond}}
	switch len(lhs) {
	case 1:
		stmt.Value = p.toIdent(lhs[0])
	case 2:
		stmt.Key, stmt.Value = p.toIdent(lhs[0]), p.toIdent(lhs[1])
	default:
		log.Panicln("TODO: parseForPhraseStmt - too many variables, 1 or 2 is required")
	}
	return stmt
}

func (p *parser) toIdent(e ast.Expr) *ast.Ident {
	switch v := e.(type) {
	case *ast.Ident:
		return v
	case *ast.BasicLit:
		p.errorExpected(e.Pos(), fmt.Sprintf("'IDENT', found %v", v.Value), 2)
	default:
		p.errorExpected(e.Pos(), "'IDENT'", 2)
	}
	return nil
}

func (p *parser) parseForPhrase() *ast.ForPhrase { // for k, v <- container if cond
	if p.trace {
		defer un(trace(p, "ForPhrase"))
	}

	pos := p.expect(token.FOR)
	p.openScope()
	defer p.closeScope()

	var k, v *ast.Ident
	v = p.parseIdent()
	if p.tok == token.COMMA { // k, v
		p.next()
		k, v = v, p.parseIdent()
	}

	tokPos := p.expect(token.ARROW) // <- container
	x := p.parseExpr(false, false, true)
	var init ast.Stmt
	var cond ast.Expr
	var ifPos token.Pos
	if p.tok == token.IF || p.tok == token.COMMA { // `condition` or `init; condition`
		ifPos = p.pos
		p.next()
		init, cond = p.parseForPhraseCond()
	}
	return &ast.ForPhrase{For: pos, Key: k, Value: v, TokPos: tokPos, X: x, IfPos: ifPos, Init: init, Cond: cond}
}

func (p *parser) parseForStmt() ast.Stmt {
	if p.trace {
		defer un(trace(p, "ForStmt"))
	}

	pos := p.expect(token.FOR)
	p.openScope()
	defer p.closeScope()

	var s1, s2, s3 ast.Stmt
	var isRange bool
	if p.tok != token.LBRACE {
		prevLev := p.exprLev
		p.exprLev = -1
		if p.tok != token.SEMICOLON {
			if p.tok == token.RANGE {
				// "for range x" (nil lhs in assignment)
				pos := p.pos
				p.next()
				y := []ast.Expr{&ast.UnaryExpr{OpPos: pos, Op: token.RANGE, X: p.parseRHSEx(true)}}
				s2 = &ast.AssignStmt{Rhs: y}
				isRange = true
			} else {
				s2, isRange = p.parseSimpleStmt(rangeOk, false)
			}
		}
		if !isRange && p.tok == token.SEMICOLON {
			p.next()
			s1 = s2
			s2 = nil
			if p.tok != token.SEMICOLON {
				s2, _ = p.parseSimpleStmt(basic, false)
			}
			p.expectSemi()
			if p.tok != token.LBRACE {
				s3, _ = p.parseSimpleStmt(basic, false)
			}
		}
		p.exprLev = prevLev
	}

	body := p.parseBlockStmt()
	p.expectSemi()

	if isRange {
		if stmt, ok := s2.(*ast.ForPhraseStmt); ok {
			stmt.For = pos
			stmt.Body = body
			return stmt
		}
		as := s2.(*ast.AssignStmt)
		// check lhs
		var key, value ast.Expr
		switch len(as.Lhs) {
		case 0:
			// nothing to do
		case 1:
			key = as.Lhs[0]
		case 2:
			key, value = as.Lhs[0], as.Lhs[1]
		default:
			p.errorExpected(as.Lhs[len(as.Lhs)-1].Pos(), "at most 2 expressions", 2)
			return &ast.BadStmt{From: pos, To: p.safePos(body.End())}
		}
		// parseSimpleStmt returned a right-hand side that
		// is a single unary expression of the form "range x"
		x := as.Rhs[0].(*ast.UnaryExpr).X
		return &ast.RangeStmt{
			For:    pos,
			Key:    key,
			Value:  value,
			TokPos: as.TokPos,
			Tok:    as.Tok,
			X:      x,
			Body:   body,
		}
	}

	// regular for statement
	return &ast.ForStmt{
		For:  pos,
		Init: s1,
		Cond: p.makeExpr(s2, "boolean or range expression"),
		Post: s3,
		Body: body,
	}
}

func (p *parser) parseStmt(allowCmd bool) (s ast.Stmt) {
	if p.trace {
		defer un(trace(p, "Statement"))
	}

	switch p.tok {
	case token.TYPE:
		s = &ast.DeclStmt{Decl: p.parseGenDecl(p.tok, p.parseTypeSpec)}
	case token.CONST, token.VAR:
		s = &ast.DeclStmt{Decl: p.parseGenDecl(p.tok, p.parseValueSpec)}
	case
		// tokens that may start an expression
		token.INT, token.FLOAT, token.IMAG, token.RAT, token.CHAR, token.STRING, token.CSTRING, token.FUNC, token.LPAREN, // operands
		token.ADD, token.SUB, token.MUL, token.AND, token.XOR, token.ARROW, token.NOT, // unary operators
		token.LBRACK, token.STRUCT, token.CHAN, token.INTERFACE: // composite types
		allowCmd = false
		fallthrough
	case token.IDENT, token.MAP: // operands
		s, _ = p.parseSimpleStmt(labelOk, allowCmd)
		// because of the required look-ahead, labeled statements are
		// parsed by parseSimpleStmt - don't expect a semicolon after
		// them
		if _, isLabeledStmt := s.(*ast.LabeledStmt); !isLabeledStmt {
			p.expectSemi()
		}
	case token.GO:
		s = p.parseGoStmt()
	case token.DEFER:
		s = p.parseDeferStmt()
	case token.RETURN:
		s = p.parseReturnStmt()
	case token.BREAK, token.CONTINUE, token.GOTO, token.FALLTHROUGH:
		s = p.parseBranchStmt(p.tok)
	case token.LBRACE:
		s = p.parseBlockStmt()
		p.expectSemi()
	case token.IF:
		s = p.parseIfStmt()
	case token.SWITCH:
		s = p.parseSwitchStmt()
	case token.SELECT:
		s = p.parseSelectStmt()
	case token.FOR:
		s = p.parseForStmt()
	case token.SEMICOLON:
		// Is it ever possible to have an implicit semicolon
		// producing an empty statement in a valid program?
		// (handle correctly anyway)
		s = &ast.EmptyStmt{Semicolon: p.pos, Implicit: p.lit == "\n"}
		p.next()
	case token.RBRACE:
		// a semicolon may be omitted before a closing "}"
		s = &ast.EmptyStmt{Semicolon: p.pos, Implicit: true}
	default:
		// no statement found
		pos := p.pos
		p.errorExpected(pos, "statement", 2)
		p.advance(stmtStart)
		s = &ast.BadStmt{From: pos, To: p.pos}
	}

	return
}

// ----------------------------------------------------------------------------
// Declarations

type parseSpecFunction func(doc *ast.CommentGroup, keyword token.Token, iota int) ast.Spec

func isValidImport(lit string) bool {
	const illegalChars = `!"#$%&'()*,:;<=>?[\]^{|}` + "`\uFFFD"
	s, _ := strconv.Unquote(lit) // go/scanner returns a legal string literal
	for _, r := range s {
		if !unicode.IsGraphic(r) || unicode.IsSpace(r) || strings.ContainsRune(illegalChars, r) {
			return false
		}
	}
	return s != ""
}

func (p *parser) parseImportSpec(doc *ast.CommentGroup, _ token.Token, _ int) ast.Spec {
	if p.trace {
		defer un(trace(p, "ImportSpec"))
	}

	var ident *ast.Ident
	switch p.tok {
	case token.PERIOD:
		ident = &ast.Ident{NamePos: p.pos, Name: "."}
		p.next()
	case token.IDENT:
		ident = p.parseIdent()
	}

	pos := p.pos
	var path string
	if p.tok == token.STRING {
		path = p.lit
		if !isValidImport(path) {
			p.error(pos, "invalid import path: "+path)
		}
		p.next()
	} else {
		p.expect(token.STRING) // use expect() error handling
	}
	p.expectSemi() // call before accessing p.linecomment

	// collect imports
	spec := &ast.ImportSpec{
		Doc:     doc,
		Name:    ident,
		Path:    &ast.BasicLit{ValuePos: pos, Kind: token.STRING, Value: path},
		Comment: p.lineComment,
	}
	p.imports = append(p.imports, spec)

	return spec
}

func (p *parser) parseValueSpec(doc *ast.CommentGroup, keyword token.Token, iota int) ast.Spec {
	if p.trace {
		defer un(trace(p, keyword.String()+"Spec"))
	}

	pos := p.pos
	idents := p.parseIdentList()
	typ := p.tryType()
	var values []ast.Expr
	// always permit optional initialization for more tolerant parsing
	if p.tok == token.ASSIGN {
		p.next()
		values = p.parseRHSList()
	}
	p.expectSemi() // call before accessing p.linecomment

	switch keyword {
	case token.VAR:
		if typ == nil && values == nil {
			p.error(pos, "missing variable type or initialization")
		}
	case token.CONST:
		if values == nil && (iota == 0 || typ != nil) {
			p.error(pos, "missing constant value")
		}
	}
	// Go spec: The scope of a constant or variable identifier declared inside
	// a function begins at the end of the ConstSpec or VarSpec and ends at
	// the end of the innermost containing block.
	// (Global identifiers are resolved in a separate phase after parsing.)
	spec := &ast.ValueSpec{
		Doc:     doc,
		Names:   idents,
		Type:    typ,
		Values:  values,
		Comment: p.lineComment,
	}
	kind := ast.Con
	if keyword == token.VAR {
		kind = ast.Var
	}
	p.declare(spec, iota, p.topScope, kind, idents...)

	return spec
}

func (p *parser) parseTypeSpec(doc *ast.CommentGroup, _ token.Token, _ int) ast.Spec {
	if p.trace {
		defer un(trace(p, "TypeSpec"))
	}

	ident := p.parseIdent()

	// Go spec: The scope of a type identifier declared inside a function begins
	// at the identifier in the TypeSpec and ends at the end of the innermost
	// containing block.
	// (Global identifiers are resolved in a separate phase after parsing.)
	spec := &ast.TypeSpec{Doc: doc, Name: ident}
	p.declare(spec, nil, p.topScope, ast.Typ, ident)
	if p.tok == token.ASSIGN {
		spec.Assign = p.pos
		p.next()
	}
	spec.Type = p.parseType()
	p.expectSemi() // call before accessing p.linecomment
	spec.Comment = p.lineComment

	return spec
}

func (p *parser) parseGenDecl(keyword token.Token, f parseSpecFunction) *ast.GenDecl {
	if p.trace {
		defer un(trace(p, "GenDecl("+keyword.String()+")"))
	}

	doc := p.leadComment
	pos := p.expect(keyword)
	var lparen, rparen token.Pos
	var list []ast.Spec
	if p.tok == token.LPAREN {
		lparen = p.pos
		p.next()
		for iota := 0; p.tok != token.RPAREN && p.tok != token.EOF; iota++ {
			list = append(list, f(p.leadComment, keyword, iota))
		}
		rparen = p.expect(token.RPAREN)
		p.expectSemi()
	} else {
		list = append(list, f(nil, keyword, 0))
	}

	return &ast.GenDecl{
		Doc:    doc,
		TokPos: pos,
		Tok:    keyword,
		Lparen: lparen,
		Specs:  list,
		Rparen: rparen,
	}
}

func isOverloadOps(tok token.Token) bool {
	return int(tok) < len(overloadOps) && overloadOps[tok] != 0
}

func (p *parser) parseFuncDeclOrCall() (*ast.FuncDecl, *ast.CallExpr) {
	if p.trace {
		defer un(trace(p, "FunctionDecl"))
	}

	doc := p.leadComment
	pos := p.expect(token.FUNC)
	scope := ast.NewScope(p.topScope) // function scope

	var recv, params, results *ast.FieldList
	var ident *ast.Ident
	var isOp, isFunLit, ok bool

	if p.tok != token.LPAREN { // func identOrOp(...)
		ident, isOp = p.parseIdentOrOp()
		params, results = p.parseSignature(scope)
	} else {
		// method: func (recv) XXX(params) results { ... }
		// funlit: func (params) results { ... }()
		params = p.parseParameters(scope, true)
		if p.tok == token.LPAREN {
			// func (params) (results) { ... }()
			isFunLit, results = true, p.parseParameters(scope, false)
		} else if isOp = isOverloadOps(p.tok); isOp {
			oldtok, oldpos := p.tok, p.pos
			p.next()
			if p.tok == token.LPAREN {
				// func (recv) op(params) results { ... }
				recv, ident = params, &ast.Ident{NamePos: oldpos, Name: oldtok.String()}
				params, results = p.parseSignature(scope)
			} else {
				// func (params) typ { ... }()
				p.unget(oldpos, oldtok, "")
				typ := p.tryType()
				if typ == nil {
					panic("TODO: invalid result type")
				}
				isFunLit, results = true, &ast.FieldList{List: []*ast.Field{{Type: typ}}}
			}
		} else if typ := p.tryType(); typ == nil {
			// func (params) { ... }()
			isFunLit = true
		} else if ident, ok = typ.(*ast.Ident); ok && p.tok == token.LPAREN {
			// func (recv) ident(params) results { ... }
			recv = params
			params, results = p.parseSignature(scope)
		} else {
			// func (params) typ { ... }()
			isFunLit, results = true, &ast.FieldList{List: []*ast.Field{{Type: typ}}}
		}
		if isFunLit {
			body := p.parseBody(scope)
			funLit := &ast.FuncLit{
				Type: &ast.FuncType{
					Func:    pos,
					Params:  params,
					Results: results,
				},
				Body: body,
			}
			call := p.parseCallOrConversion(funLit, false)
			p.expectSemi()
			return nil, call
		}
	}

	if isOp {
		if params == nil || len(params.List) != 1 {
			log.Panicln("TODO: overload operator can only have one parameter")
		}
	}
	var body *ast.BlockStmt
	if p.tok == token.LBRACE {
		body = p.parseBody(scope)
		p.expectSemi()
	} else if p.tok == token.SEMICOLON {
		p.next()
		if p.tok == token.LBRACE {
			// opening { of function declaration on next line
			p.error(p.pos, "unexpected semicolon or newline before {")
			body = p.parseBody(scope)
			p.expectSemi()
		}
	} else {
		p.expectSemi()
	}

	decl := &ast.FuncDecl{
		Doc:  doc,
		Recv: recv,
		Name: ident,
		Type: &ast.FuncType{
			Func:    pos,
			Params:  params,
			Results: results,
		},
		Body:     body,
		Operator: isOp,
	}
	if recv == nil {
		// Go spec: The scope of an identifier denoting a constant, type,
		// variable, or function (but not method) declared at top level
		// (outside any function) is the package block.
		//
		// init() functions cannot be referred to and there may
		// be more than one - don't put them in the pkgScope
		if ident.Name != "init" {
			p.declare(decl, nil, p.pkgScope, ast.Fun, ident)
		}
	}
	if debugParseOutput {
		log.Printf("ast.FuncDecl{Name: %v, ...}\n", ident.Name)
	}
	return decl, nil
}

func (p *parser) parseDecl(sync map[token.Token]bool) ast.Decl {
	if p.trace {
		defer un(trace(p, "Declaration"))
	}
	var f parseSpecFunction
	pos := p.pos
	switch p.tok {
	case token.CONST, token.VAR:
		f = p.parseValueSpec
	case token.TYPE:
		f = p.parseTypeSpec
	case token.FUNC:
		decl, call := p.parseFuncDeclOrCall()
		if decl != nil {
			if p.errors.Len() != 0 {
				p.errorExpected(pos, "declaration", 2)
				p.advance(sync)
			}
			return decl
		}
		return p.parseGlobalStmts(sync, pos, &ast.ExprStmt{X: call})
	default:
		return p.parseGlobalStmts(sync, pos)
	}
	return p.parseGenDecl(p.tok, f)
}

func (p *parser) parseGlobalStmts(sync map[token.Token]bool, pos token.Pos, stmts ...ast.Stmt) *ast.FuncDecl {
	p.topScope = ast.NewScope(p.topScope)
	doc := p.leadComment
	p.openLabelScope()
	list := p.parseStmtList()
	p.closeLabelScope()
	p.closeScope()
	if stmts != nil {
		list = append(stmts, list...)
	}
	p.noEntrypoint = true
	if p.errors.Len() != 0 { // TODO: error
		p.advance(sync)
	}
	return &ast.FuncDecl{
		Name: &ast.Ident{NamePos: pos, Name: "main"},
		Doc:  doc,
		Type: &ast.FuncType{
			Params: &ast.FieldList{},
		},
		Body: &ast.BlockStmt{List: list},
	}
}

// ----------------------------------------------------------------------------
// Source files

func (p *parser) parseFile() *ast.File {
	if p.trace {
		defer un(trace(p, "File"))
	}

	// Don't bother parsing the rest if we had errors scanning the first token.
	// Likely not a Go source file at all.
	if p.errors.Len() != 0 {
		return nil
	}

	var noPkgDecl bool
	// package clause
	doc := p.leadComment
	var pos token.Pos
	var ident *ast.Ident
	if p.tok == token.PACKAGE {
		pos = p.expect(token.PACKAGE)
		// Go spec: The package clause is not a declaration;
		// the package name does not appear in any scope.
		ident = p.parseIdent()
		if ident.Name == "_" && p.mode&DeclarationErrors != 0 {
			p.error(p.pos, "invalid package name _")
		}
		p.expectSemi()

		// Don't bother parsing the rest if we had errors parsing the package clause.
		// Likely not a Go source file at all.
		if p.errors.Len() != 0 {
			return nil
		}
	} else {
		noPkgDecl = true
		pos = token.NoPos
		ident = ast.NewIdent("main")
	}

	p.openScope()
	p.pkgScope = p.topScope
	var decls []ast.Decl
	if p.mode&PackageClauseOnly == 0 {
		// import decls
		for p.tok == token.IMPORT {
			decls = append(decls, p.parseGenDecl(token.IMPORT, p.parseImportSpec))
		}

		if p.mode&ImportsOnly == 0 {
			// rest of package body
			for p.tok != token.EOF {
				decls = append(decls, p.parseDecl(declStart))
			}
		}
	}
	p.closeScope()
	assert(p.topScope == nil, "unbalanced scopes")
	assert(p.labelScope == nil, "unbalanced label scopes")

	// resolve global identifiers within the same file
	i := 0
	for _, ident := range p.unresolved {
		// i <= index for current ident
		assert(ident.Obj == unresolved, "object already resolved")
		ident.Obj = p.pkgScope.Lookup(ident.Name) // also removes unresolved sentinel
		if ident.Obj == nil {
			p.unresolved[i] = ident
			i++
		}
	}

	return &ast.File{
		Doc:          doc,
		Package:      pos,
		Name:         ident,
		Decls:        decls,
		Scope:        p.pkgScope,
		Imports:      p.imports,
		Unresolved:   p.unresolved[0:i],
		Comments:     p.comments,
		NoEntrypoint: p.noEntrypoint,
		NoPkgDecl:    noPkgDecl,
	}
}
