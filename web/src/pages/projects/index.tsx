import { type Project } from '@buf/pushpa_cotton.bufbuild_es/projects/v1/projects_pb'
import { ConnectError } from '@connectrpc/connect'
import { useEffect, useState } from 'react'
import { Link } from 'wouter'
import { AppSidebar } from '@/components/nav/app-sidebar'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { SidebarProvider, SidebarInset, SidebarTrigger } from '@/components/ui/sidebar'
import { Spinner } from '@/components/ui/spinner'
import { projectsService } from '@/lib/rpc'

function Projects() {
  const [projects, setProjects] = useState<Project[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    const fetchProjects = async () => {
      try {
        setLoading(true)
        const response = await projectsService.batchGet({})
        setProjects(response.projects)
      } catch (err) {
        if (err instanceof ConnectError) {
          setError(err.rawMessage)
        } else {
          setError(err instanceof Error ? err.message : 'An error occurred while fetching projects')
        }
      } finally {
        setLoading(false)
      }
    }

    fetchProjects()
  }, [])

  return (
    <SidebarProvider>
      <AppSidebar />
      <SidebarInset>
        <header className="flex h-16 items-center gap-4 border-b bg-background px-4 sm:px-6 lg:px-8">
          <SidebarTrigger />
          <div className="flex-1 flex justify-between items-center">
            <h1 className="text-xl font-semibold">Projects</h1>
            <Link to="/projects/new">
              <Button variant="default">Create Project</Button>
            </Link>
          </div>
        </header>
        <main className="flex-1 p-4 sm:p-6 lg:p-8">
          <div className="max-w-4xl mx-auto">
            <h2 className="text-2xl font-bold mb-4">Your Projects</h2>
            {loading ? (
              <div className="flex justify-center items-center h-64">
                <Spinner className="h-8 w-8" />
              </div>
            ) : error ? (
              <div className="text-destructive p-4 text-center">
                <p>Error: {error}</p>
              </div>
            ) : projects.length === 0 ? (
              <div className="text-center p-8">
                <p className="text-muted-foreground">No projects found. Create your first project to get started.</p>
              </div>
            ) : (
              <div className="space-y-4">
                {projects.map((project) => (
                  <Link key={project.id} to={`/projects/${project.id}`} className="block">
                    <Card className="hover:shadow-md transition-shadow">
                      <CardHeader>
                        <CardTitle className="text-lg">{project.displayName}</CardTitle>
                      </CardHeader>
                      <CardContent>
                        <p className="text-sm text-muted-foreground break-all">API Key: {project.apiKey}</p>
                      </CardContent>
                    </Card>
                  </Link>
                ))}
              </div>
            )}
          </div>
        </main>
      </SidebarInset>
    </SidebarProvider>
  )
}

export default Projects