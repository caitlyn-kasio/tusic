package main

import (
	"bytes"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)


func main(){
	
	// Initialize app, create root
	app := tview.NewApplication()
	flexRoot := tview.NewFlex().
		SetDirection(tview.FlexColumn)

	// Browser Handling
	
	rootDir := "/home/caitlyn/Music/"	// TODO: Add args to this!
	rootNode := tview.NewTreeNode(rootDir)
	
	// Final Construction of browserTree
	browserTree := tview.NewTreeView().
		SetRoot(rootNode).
		SetCurrentNode(rootNode)
	
		
	checkIsDir := func(node *tview.TreeNode) bool{
		_fileRef := node.GetReference().(string)
		_file, err := os.Open(_fileRef); if err != nil{
			panic(err)
		}
		_fileStat, err := _file.Stat(); if err != nil{
			panic(err)
		}
		if _fileStat.IsDir(){
			return true
		}
		return false
	}
		
		
		// Thank you to the example tree view for tview!
	browserAdd := func(target *tview.TreeNode, path string){
		files, err := ioutil.ReadDir(path)
		if err != nil{
			panic(err)
		}
		
		for _, file := range files {
			node := tview.NewTreeNode(file.Name()).
				SetReference(filepath.Join(path, file.Name())).
				SetSelectable(true)
			
			if file.IsDir(){
				node.SetColor(tcell.ColorBlue)
			}
			target.AddChild(node)
		}
	}
	
	browserAdd(rootNode, rootDir)
	
	
	browserExpand := func(node *tview.TreeNode) {
		reference := node.GetReference()
		if reference == nil {
			return // Selecting the root node does nothing.
		}

		if node.GetColor() != tcell.ColorBlue{
			return
		}
		children := node.GetChildren()
		if len(children) == 0 {
			// Load and show files in this directory.
			path := reference.(string)
			browserAdd(node, path)
		} else {
			// Collapse if visible, expand if collapsed.
			node.SetExpanded(!node.IsExpanded())
		}
	}
	
	
	// Initialize primitives before adding
	metadataDisplay := tview.NewTextView()
	controlDisplay := tview.NewTextView().SetBorder(true).SetTitle(" Keybindings ")
	
	
	// We set up nesting in reverse, which is confusing
	rightFlexBox := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(metadataDisplay,0,5,false).
		AddItem(controlDisplay, 0,1,false)
		
	// Main layout construction
	flexRoot.AddItem(browserTree, 0, 1,false).
		AddItem(rightFlexBox, 0, 2, false)
	
	
	browserTree.SetTitle(" Browser ")
	browserTree.SetBorder(true)
	browserTree.SetTopLevel(1)
	
	metadataDisplay.SetTitle(" Metadata ")
	metadataDisplay.SetBorder(true)
		
		
	
	// Key Input Handling
	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey{
		switch event.Rune(){
			// Close program
			case 'q':
				panic("Program Quit")
// 			// Tree Navigation
			case 'w':
				browserTree.Move(-1)
			case 's':
				browserTree.Move(1)
			case 'd':
				_cur := browserTree.GetCurrentNode()
				// Expand folder
				if checkIsDir(_cur){
					browserExpand(_cur)
				}
			case 'p':
				// Play
				_cur := browserTree.GetCurrentNode()
				_isDir := true
				if checkIsDir(_cur){
					_isDir = true
				}else{
					_isDir = false
				}
				//Play Folder
				if _isDir{
					metadataDisplay.SetText("is Dir")
				// Play File
				}else{
					_payload := (_cur.GetReference().(string))
// 					_payload = strings.ReplaceAll(_payload, " ", "\\ ")

					metadataDisplay.SetText(_payload)
					
					cmd := exec.Command("paplay", _payload)
					var stderr bytes.Buffer
					cmd.Stderr = &stderr

					if err := cmd.Start(); err != nil {
						panic(err)
					}
					
					go func(){
						if err := cmd.Wait(); err != nil{
							log.Printf("paplay failed: %v\nstderr: %s", err, stderr.String())
						}
					}()


					
					
				}
				
				
				
		}
		return event
	})
	
		
		
	// Run the app
	if err := app.SetRoot(flexRoot, true).Run(); err != nil{
		panic(err)
	}
	
}